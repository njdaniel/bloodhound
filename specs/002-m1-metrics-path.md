# 002 — M1: The Metrics Path

**Scope:** mcp-prom, metrics-hound, orchestrator v0 (intake → one hound → report).
**Milestone:** M1 (spec 001 §6). **Status:** draft v0.1 — 2026-07-28.
**Demo target:** `bloodhound hunt --alert fixture.json` produces a real, metrics-only
diagnosis of a manually broken pod against a live Prometheus.

---

## 1. Problem

M0 ships types and stubs. M1 must prove the three load-bearing mechanisms of the
whole system on the narrowest possible slice:

1. An MCP server that returns **bounded, model-shaped** data (mcp-prom).
2. An LLM **tool-use loop** with hard budgets that ends in schema-valid
   structured output (metrics-hound).
3. A **checkpointed state machine** that survives a crash mid-run
   (orchestrator v0).

Everything M2+ adds (more hounds, planner, verifier) is horizontal repetition of
these three mechanisms. Get the shapes right here, cheaply, on one path.

**M1 scoping decisions** (deviations from the 001 component descriptions, all
deferred not cut):

- Intake is the **CLI file path only** (`hunt --alert <file>`). The Alertmanager
  webhook server (`serve`) lands in M2 with dedup.
- Orchestrator v0 runs **intake → investigate → report**. `plan` and `verify`
  phases stay reserved in the `Phase` enum; the v0 transition table skips them.
- Report output is terminal + JSON. Slack is M3.
- OTel spans are M2; M1 captures timing/spend in checkpoints so the data exists.

## 2. mcp-prom

Standalone binary `mcp/prom/`, stdio transport, official
`modelcontextprotocol/go-sdk`. Talks to Prometheus via its HTTP API using
`net/http` + `encoding/json` directly — the query API is small and stable, and
this avoids a `client_golang` dependency (stdlib-first rule; revisit if we need
remote-write in tests).

Configuration via flags/env: `PROM_URL` (required), `PROM_TIMEOUT` (default
15s). No auth in v1 (bench cluster is local).

### 2.1 Guardrail constants

One table, shared by all tools, defined in one place in code:

| Constant | Value | Applied to |
|---|---|---|
| `QueryTimeout` | 15s | every upstream HTTP call (context deadline) |
| `MaxRange` | 24h | `query_range` end−start; violations are an **error, not a clamp** — the model is told to narrow the window |
| `MaxPointsPerSeries` | 120 | step clamping (§2.3) |
| `MaxSeries` | 15 | `query_range` result cap |
| `MaxInstantSamples` | 100 | `query_instant` result cap |
| `MaxAlerts` | 50 | `list_alerts` |
| `MaxMetadataMetrics` | 25 | `series_metadata` |
| `MaxLabelValues` | 10 | per label key in `series_metadata` |
| `MetadataLookback` | 1h | `series_metadata` discovery window: `start`/`end` on `/api/v1/series`, so Prometheus does not scan full retention |
| `MaxUpstreamSeries` | 2000 | `limit` sent to `/api/v1/series` by `series_metadata` |
| `MaxUpstreamMetadata` | 2000 | `limit` sent to `/api/v1/metadata` by `series_metadata` |
| `MaxStringLen` | 120 | any label value / metadata string (truncate to 117 + `…`) |
| `MaxAlertAnnotationLen` | 200 | alert annotation values |
| `MaxQueryAnnotations` | 5 | PromQL annotations per severity in `query_range` / `query_instant` (§2.4) |
| `MaxQueryAnnotationLen` | 200 | one PromQL annotation; above `MaxStringLen` because the metric name and source position sit at the end of a 132–141-byte string |
| `MaxResponseBytes` | 32 KiB | serialized tool result; triggers point-thinning (§2.3) |
| `FloatFormat` | `%.6g` | every sample value |

Every truncation is **marked in the payload** — the model must never mistake a
capped result for a complete one. That marking is itself prompt engineering.

### 2.2 Tool surface

All results are one MCP text content block containing JSON. Errors that the
model can act on (bad PromQL, window too wide) are returned as MCP tool errors
with an actionable message ("range 36h exceeds 24h limit; narrow start/end"),
not protocol failures.

#### `query_range`

Input schema:

```json
{
  "type": "object",
  "required": ["query", "start", "end"],
  "additionalProperties": false,
  "properties": {
    "query": { "type": "string", "description": "PromQL expression" },
    "start": { "type": "string", "format": "date-time", "description": "RFC 3339" },
    "end":   { "type": "string", "format": "date-time", "description": "RFC 3339" },
    "step_seconds": { "type": "integer", "minimum": 1,
      "description": "Requested resolution. The server clamps so a series has at most 120 points; the effective step is echoed back." }
  }
}
```

Output:

```json
{
  "resolved_step_seconds": 60,
  "series": [
    {
      "labels": { "pod": "checkout-7d9f...", "namespace": "shop" },
      "stats": { "min": 0.1, "max": 42.7, "avg": 3.2, "last": 41.9 },
      "points": [[1753700000, "3.14159"], ["…"]]
    }
  ],
  "truncation": {
    "series_total": 42,
    "series_returned": 15,
    "points_thinned": false,
    "note": "27 series dropped; ranked by (max-min) descending. Narrow the selector to see specific series."
  },
  "annotations": { "…": "§2.4, omitted entirely when Prometheus raised none" }
}
```

`stats` exists so the model can reason about a series without reading every
point — often it never needs to.

#### `query_instant`

Input: `{ "query": string (required), "time": RFC3339 (optional, default now) }`.
Output: `{ "result_type": "vector"|"scalar", "samples": [{ "labels": {...}, "value": "1.5", "timestamp": 1753700000 }], "truncation": {...}, "annotations": {...} }`.
Cap `MaxInstantSamples`, ranked by |value| descending (you almost always want
the outliers), deterministic tie-break on sorted labelset.

#### `list_alerts`

Input: `{ "state": "firing"|"pending"|"all" }` (optional, default `"firing"`).
Output: `{ "alerts": [{ "name", "state", "active_at", "value", "labels": {...}, "annotations": {...} }], "truncation": {...} }`.
Sorted by `active_at` descending, cap `MaxAlerts`; annotation values truncated
to `MaxAlertAnnotationLen`.

#### `series_metadata`

Input: `{ "match": string (required, series selector e.g. '{namespace="shop"}' or metric name) }`.
Output per metric: name, type, help (truncated), label keys with up to
`MaxLabelValues` sample values each. Backed by `/api/v1/metadata` +
`/api/v1/series`. Cap `MaxMetadataMetrics` metrics, alphabetical (discovery
tool — determinism beats relevance ranking here).

**Upstream cost is bounded too, not just the output.** `/api/v1/series` is
called with `start = now − MetadataLookback`, `end = now` and
`limit = MaxUpstreamSeries`; `/api/v1/metadata` with
`limit = MaxUpstreamMetadata`. Without the window Prometheus scans full
retention, and without the metadata limit it describes every metric it knows —
both to produce a result capped at 25 metrics. `limit` is advisory: Prometheus
versions that predate it ignore the parameter, so the client-side caps stay
authoritative and the output is bounded either way.

Three consequences, all deliberate, all marked:

- A metric with no samples in the last `MetadataLookback` is invisible to
  discovery. The tool description says so explicitly, because a model that
  reads absence as non-existence will stop investigating a real metric.
- When the server honours the series limit, the result may be missing whole
  metrics. Detected from the response's `warnings` (`"results truncated due to
  limit"`), **not** from the result count: a server old enough to ignore
  `limit` returns everything, and counting would report a truncation that
  never happened. Only a warning that says *truncated* produces that note;
  any other warning (a Thanos/Cortex partial response, a failing
  `remote_read`) is surfaced verbatim instead, since blaming it on the series
  limit would give the model a fabricated cause and advice that cannot help.
  Marked in `truncation.note`.
- When the server honours the metadata limit, metrics come back with an empty
  `type` and `help`. `/api/v1/metadata` does not warn, and it fills its map by
  walking active targets until the limit, so *which* metrics keep their
  metadata is arbitrary and can differ between two calls. A full map is
  therefore reported as possibly truncated, but only when a returned metric is
  actually missing its `type` or `help` — otherwise that empty field, which
  used to mean "no metadata registered", becomes a nondeterministic maybe on
  exactly the large Prometheus these bounds exist for.

The truncation note joins one sentence per applied cap and appends the advice
they share ("Narrow the match selector.") once.

### 2.3 Exact truncation strategy (`query_range`)

Applied in this order; each step is deterministic:

1. **Range check.** `end − start > MaxRange` → tool error (model narrows and
   retries). No silent clamping of the window: a silently shrunk window would
   make the model reason about a different time span than it asked for.
2. **Step clamp.** `effective_step = max(step_seconds, ceil((end−start)/MaxPointsPerSeries))`,
   default `ceil((end−start)/MaxPointsPerSeries)` if omitted. Echoed as
   `resolved_step_seconds`.
3. **Query** with `QueryTimeout`.
4. **Series cap.** If series count > `MaxSeries`: rank by `(max − min)`
   descending (volatility is what an investigator wants), tie-break by max |value|
   descending, then lexicographically by sorted labelset (full determinism —
   golden tests depend on it). Keep top `MaxSeries`; record `series_total` /
   `series_returned` and the ranking rule in `truncation.note`.
5. **Value formatting.** Timestamps as unix seconds; values as strings via
   `%.6g` (strings dodge float JSON round-trip noise in goldens).
6. **Size backstop.** If the serialized result still exceeds `MaxResponseBytes`,
   thin points: keep first and last, drop every second interior point; repeat
   until it fits. Set `points_thinned: true` and note the retained resolution.
   `stats` are always computed from the **full-resolution** data, before thinning.

Known bias, accepted for v1: ranking by `(max − min)` favors large-magnitude
series (a 0→1 error-ratio flip loses to a 10k-req/s wobble). The mitigation is
the hound's prompt: prefer rate/ratio queries and narrow selectors over broad
matches. Revisit with a normalized score only if evals show it matters.

### 2.4 Upstream PromQL annotations (`query_range`, `query_instant`)

Prometheus attaches non-fatal annotations to a successful evaluation, in two
arrays with different severities. Measured against v3.5.0:

```
warnings: PromQL warning: bucket label "le" is missing or has a malformed
          value of "" for metric name "…" (1:25)
infos:    PromQL info: metric might not be a counter, name does not end in
          _total/_sum/_count/_bucket: "…" (1:6)
```

A warning means the numbers in the payload may be meaningless; an info means
the expression is a likely mistake though the result is well-defined. Both are
carried to the model in an `annotations` block:

```json
{
  "warnings": ["PromQL warning: …"],
  "warnings_total": 8,
  "infos": ["PromQL info: …"],
  "infos_total": 1,
  "note": "3 further warnings dropped; kept one of each distinct kind first, then alphabetically. …"
}
```

Five decisions, all deliberate:

- **Beside `truncation`, not inside it.** `truncation` answers "how much of the
  answer am I seeing"; an annotation answers "is this an answer at all", and
  nothing was dropped when Prometheus reports a malformed bucket label. Folding
  it in would also bury it, since a model that has learned `truncation` is about
  caps will skip the block precisely when `series_total == series_returned` —
  the case where "the quantile you just computed is meaningless" matters most.
- **Omitted entirely when the server was quiet**, so presence is the signal and
  a well-formed query's payload is unchanged from before this existed.
- **Severities stay in separate arrays**, mirroring the wire, because they call
  for different actions. Flattening would leave the model re-deriving severity
  from a `"PromQL warning:"` prefix the server need not keep.
- **Verbatim, not classified.** This is the other half of the `series_metadata`
  rule in §2.2: classify a warning that describes a cap *bloodhound asked for*
  (there, `limit=MaxUpstreamSeries`) and pass everything else through. These
  tools send no limit, so every annotation is a PromQL diagnostic whose value is
  its exact wording — the metric name and the `(line:col)` position are the only
  parts that say which query to fix.
- **Ordered before capping at `MaxQueryAnnotations`, then capped one kind at a
  time.** Prometheus accumulates annotations in a map and serializes them in map
  iteration order, so identical calls return the same annotations differently
  ordered (the integration test measures the spread each run); keeping the first
  N of that would hand the model a different subset every call. But plain
  alphabetical order is biased, not merely arbitrary: the per-metric template
  starts with `bucket label` and almost every other template starts later in the
  alphabet, so the near-identical repeats sort first every time and the cap drops
  the annotation that says something new. `histogram_quantile(1.5, {job="…"})`
  returns eight `bucket label` repeats plus one `quantile value should be between
  0 and 1, got 1.5`, and the five alphabetically first are all repeats.
  Annotations are therefore grouped by kind — the message up to its first quoted
  operand or number, i.e. the template — and one of each kind is taken before a
  second of any, so while there are no more kinds than slots every kind is
  represented. The kept set is sorted for presentation and is a function of the
  set, never of upstream order. Strings are capped at `MaxQueryAnnotationLen`;
  the count cap is marked in `note` like every other.

`/api/v1/alerts` and `/api/v1/metadata` are out of scope: annotations come from
PromQL evaluation and neither endpoint evaluates an expression. Measured against
v3.5.0, neither response carries a `warnings` or `infos` key at all, and the
integration suite pins that so the scope decision fails loudly if it stops
holding.

## 3. metrics-hound

`internal/agents/hounds/metrics.go` (+ shared loop plumbing in
`internal/agents/hounds/`). One exported entrypoint:

```
Run(ctx, deps, case, focus, budget) (Finding, Spend, error)
```

where `deps` carries the `llm.Provider` and an MCP session from
`internal/mcpclient`. M1's `focus` is derived mechanically from the alert
(name + labels + firing time); the planner takes over authorship in M2.

### 3.1 Prerequisite: tool use in `internal/llm`

The M0 `Provider` has no tool surface (deferred by design — see
`provider.go:20`). M1 adds, provider-agnostically:

- `Tool{Name, Description, InputSchema json.RawMessage}`
- `ToolUse{ID, Name, Input json.RawMessage}` on `Response`
- `Message` content becomes blocks (text | tool_use | tool_result) rather than
  a bare string
- `StopReason` (`end_turn` | `tool_use` | `max_tokens`)
- `Request.ToolChoice` (`auto` | `required` | specific tool) — needed to force
  finding submission (§3.2)

`internal/llm/anthropic` implements this via `anthropic-sdk-go` (dependency
justified: first-class provider, official SDK, spec 001 §3.3). Middleware
(retry-with-backoff, token/cost accounting, request/response capture to the
case work dir) wraps `Provider` as decorators.

### 3.2 The loop

Tools offered to the model: the four mcp-prom tools (definitions fetched from
the live MCP session, so server tool descriptions are the single source of
truth) **plus one synthetic tool** `submit_finding` whose input schema is
*derived* from the Finding schema (§3.3). The hound terminates by calling it —
structured output via forced tool use, not by parsing prose.

The derivation exists because the model must not author `capture_ref` (§3.4):
the model-facing schema replaces that evidence field with a required
`tool_call_index` integer, and carries every other constraint through
unchanged, so a cap added to the checked-in asset reaches the model
automatically. One checked-in asset, two shapes. (Amended after M1
implementation: the original text said the schema *is* the Finding schema,
which contradicted §3.4.)

```
messages ← [system prompt, user turn: case + focus questions]
loop:
  if budget exhausted (tokens ≥ max ∨ tool_calls ≥ max ∨ wall clock over):
      final call with ToolChoice=submit_finding and an appended user note:
      "Budget exhausted. Submit your best-effort finding now; unverified
       theories belong in dead_ends."
  resp ← provider.Complete(...)
  for each ToolUse in resp:
      submit_finding → validate against schema
                       valid   → return Finding
                       invalid → tool_result carries the validation errors
                                 (max 2 repair attempts, then hard error)
      mcp tool      → execute via mcpclient (per-call timeout), capture
                      request+response to workdir, append tool_result
                      (tool errors are returned to the model as tool_results —
                       bad PromQL is the model's problem to fix)
  no ToolUse (prose only) → nudge once ("investigate with tools, finish with
      submit_finding"); second offense → ToolChoice=required
```

Budget defaults (config-overridable): **12 tool calls, 50k total tokens, 3m
wall clock.** Every loop iteration checks `ctx` first; the orchestrator owns
the phase-level deadline.

The system prompt (checked in as a versioned asset, not a string literal built
at runtime) instructs: establish onset time first; prefer `rate()`/ratio forms;
narrow selectors before widening; record ruled-out theories as dead ends;
never assert a claim without a query behind it.

### 3.3 Finding schema

Evolves the M0 stub (`orchestrator.Finding` — evidence/dead-ends as bare
strings) into structured records. Pre-1.0, no compatibility shim; the Go type
and this JSON schema change together.

```json
{
  "$id": "bloodhound/finding/v1",
  "type": "object",
  "required": ["hound", "summary", "confidence", "evidence", "dead_ends"],
  "additionalProperties": false,
  "properties": {
    "hound": { "const": "metrics" },
    "summary": { "type": "string", "minLength": 1, "maxLength": 1200,
      "description": "The hypothesis and the shape of the supporting data." },
    "confidence": { "type": "number", "minimum": 0, "maximum": 1 },
    "onset": { "type": "string", "format": "date-time",
      "description": "When the anomaly began, if determined." },
    "evidence": {
      "type": "array", "maxItems": 12,
      "items": {
        "type": "object",
        "required": ["tool", "query", "observation", "capture_ref"],
        "additionalProperties": false,
        "properties": {
          "tool": { "type": "string" },
          "query": { "type": "string", "maxLength": 500 },
          "observation": { "type": "string", "maxLength": 500 },
          "capture_ref": { "type": "string",
            "description": "Capture filename under the case work dir, e.g. mcp/003-query_range.json. Injected by the loop, not the model (§3.4). The model-facing submit_finding schema replaces this field with a required tool_call_index integer — see §3.2." }
        }
      }
    },
    "dead_ends": {
      "type": "array", "maxItems": 8,
      "items": {
        "type": "object",
        "required": ["theory", "ruled_out_by"],
        "additionalProperties": false,
        "properties": {
          "theory": { "type": "string", "maxLength": 300 },
          "ruled_out_by": { "type": "string", "maxLength": 300 }
        }
      }
    }
  }
}
```

### 3.4 Evidence refs are loop-assigned

The model cites evidence by tool-call **sequence number** (the loop tells it
each call's index in the tool result); the loop resolves indices to capture
filenames when validating the finding. The model never invents a
`capture_ref` — an unresolvable index is a validation error. This is the hook
the M4 grader's evidence-honesty check hangs off, so it must be mechanical
from day one.

## 4. Orchestrator v0

### 4.1 State transitions

```
        ┌─────────┐     ┌──────────────┐     ┌─────────┐     ┌──────┐
  ───▶  │ intake  │ ──▶ │ investigate  │ ──▶ │ report  │ ──▶ │ done │
        └─────────┘     └──────────────┘     └─────────┘     └──────┘
             │                 │                  │
             └────────────── failed ◀─────────────┘
```

- Linear, no branches in v0. `plan` and `verify` remain declared in the
  `Phase` enum; the v0 transition table simply doesn't visit them. The
  transition table is **data** (`map[Phase]Phase` per pipeline version), so M2
  inserts phases without touching the walk logic.
- Each phase is a function `(ctx, *run) (output any, err error)` with a
  per-phase timeout (intake 10s, investigate = the hound's **remaining**
  wall-clock budget + 30s grace, report 30s). Remaining means the cap minus the
  wall clock already recorded against that phase, so a resume gets a deadline
  sized to the budget it actually has left rather than to a fresh case's cap
  (§4.3). With the cap disabled there is no remainder, and the default cap plus
  the grace window is used as a backstop.
- A phase error → write a `failed` checkpoint (with the error), set case phase
  to `failed`, exit non-zero. **No phase-level retries in v0** — transient
  fault handling lives in the llm middleware; a phase that still fails is a
  bug or an outage, and either way stopping is honest. A failed case is
  resumable (§4.3) once the cause is fixed.
- `failed` is terminal for the process but not for the case ID.

Phase contents in M1: **intake** parses one Alertmanager-format alert JSON
into `Case`, assigns the case ID (`c-<utc yyyymmddThhmmss>-<6 hex crypto/rand>`),
creates the work dir. **investigate** runs metrics-hound (§3). **report**
renders `report.json` + pretty terminal output from the Finding (hypothesis,
confidence, evidence table, dead ends, spend footer).

### 4.2 Work dir and checkpoint format

```
work/<case-id>/
├── case.json                     # Case incl. current phase — the resume cursor
├── checkpoints/
│   ├── 01-intake.json
│   ├── 02-investigate.json
│   └── 03-report.json
├── captures/
│   ├── llm/000-metrics-hound.json     # request+response pairs, in order
│   └── mcp/000-query_range.json       # tool call + result, in order
├── report.json
└── report.txt
```

`case.json` is the serialized `orchestrator.Case`: the alert as intake parsed
it, plus the cursor a resume reads.

```json
{
  "id": "c-20260728T101500-a1b2c3",
  "alert_name": "PodCrashLooping",
  "labels": { "namespace": "shop", "pod": "checkout-7d9f4b8c6-x2ktn" },
  "firing_since": "2026-07-28T10:12:30Z",
  "phase": "investigate",
  "work_dir": "work/c-20260728T101500-a1b2c3",
  "alert_path": "fixture.json",
  "pipeline": "v0"
}
```

- `phase` is the phase to run **next** — not the one that last completed — or
  `done`/`failed`. That is what makes the file a cursor.
- `alert_name`, `labels` and `firing_since` are filled by the intake phase.
  None of them are `omitempty`, so a case file written before intake completes
  carries their zero values (`""`, `null`, `0001-01-01T00:00:00Z`) rather than
  omitting the keys.
- `work_dir` is recorded for readers of the file; a resume uses the work root
  and case ID it was invoked with instead, so a copied work dir still resumes.
  A *renamed* directory resumes under its new name while `id` keeps the old
  one — `Resume` backfills `id` only when it is empty.
- `alert_path` is the alert file the case was opened from, kept so that a
  resume can re-run a failed intake against the corrected file without the
  operator having to re-supply it (§4.3).
- `pipeline` is the version of the transition table this case is being walked
  with (`v0` in M1) — the field the §4.3 resume refusal turns on.

`alert_path` and `pipeline` are `omitempty`: an unset value is an absent key,
not an empty string. An absent `pipeline` is a mismatch, not a wildcard (§4.3).

Checkpoint file schema (`schema_version` guards future migrations):

```json
{
  "schema_version": 1,
  "case_id": "c-20260728T101500-a1b2c3",
  "phase": "investigate",
  "status": "completed",
  "started_at": "2026-07-28T10:15:00Z",
  "finished_at": "2026-07-28T10:17:42Z",
  "spend": {
    "input_tokens": 31240, "output_tokens": 2210,
    "usd": 0.0841, "tool_calls": 7, "wall_ms": 162044
  },
  "output": { "…phase-specific: intake → Case; investigate → Finding; report → artifact paths…" },
  "error": null
}
```

**Atomicity:** every JSON file in the work dir is written via
write-temp-file → fsync → `os.Rename` in the same directory. A crash leaves
either the previous version or the new one, never a torn file. `case.json` is
updated only *after* the phase checkpoint lands, in this order:
checkpoint(N) → case.json(phase=N+1). Spend totals for `bloodhound cost` are
derived by summing checkpoints — no separate ledger to drift.

### 4.3 Resume

`bloodhound hunt --resume <case-id>` (same binary path the M2 `replay` command
will build on):

1. Read `case.json`. If the `pipeline` version it records differs from the
   version the running orchestrator walks, **refuse the case** — the resume
   fails and writes nothing: no checkpoint, no `case.json` update, no
   captures. Checkpoint *filenames* carry the walk index (`02-investigate.json`),
   so walking a different table over old checkpoints would shift a phase's
   index, orphan the file written under the old one, and make the checkpoint
   sum double-count that phase's spend. There is deliberately **no migration
   path**: refusing is the whole behaviour, and a case file that records no
   pipeline version at all is a mismatch too. Because the refusal is permanent,
   the CLI reports it as **exit 2**, not the exit 1 of a resumable phase
   failure — an operator wrapper that retries exit 1 must not loop on a case
   that can never be resumed.
2. Enumerate `checkpoints/`.
3. Every phase with a `completed` checkpoint is **loaded, never re-executed** —
   its `output` is deserialized as that phase's result.
4. Walking the transition table, the first phase without a completed
   checkpoint runs next (a `failed` checkpoint is overwritten on re-run).
5. Spend already recorded is counted toward budgets on resume — a crash loop
   must not multiply cost.

Acceptance test for this section, literally: `kill -9` mid-investigate,
resume, and the LLM/MCP capture sequence numbers continue from where they
stopped rather than restarting at 000.

## 5. Test plan

House rule framing: unit + golden tests are the coverage; the Prometheus
container test is a smoke check; nothing in CI's default path calls a paid API.

**mcp-prom**
- Unit tests against `httptest` fake Prometheus (canned `/api/v1/*` JSON):
  step-clamp arithmetic (table-driven: range/step combinations incl. omitted
  step); series ranking + tie-breaks (crafted equal-range series → assert
  lexicographic order); size backstop (fixture > 32 KiB → thinned, first/last
  points retained, `stats` unchanged); range-limit and bad-PromQL paths return
  MCP tool errors with the actionable message.
- Golden tests: full serialized tool results for fixed fixtures, byte-exact
  (this is what makes truncation "exact" — determinism is enforced, not hoped).
- MCP conformance: spawn the built binary over stdio with the go-sdk client;
  list tools, assert names/schemas; call each tool once against the fake.
- Integration (`-tags integration`, `make test-integration`): real Prometheus
  container (host network) scraping a test-owned `/metrics` endpoint at 250ms
  interval; poll until samples land; assert real query shapes. Runs in CI on
  PRs (no API key involved).

**metrics-hound**
- `internal/llm/llmtest`: scripted provider (queue of canned responses,
  records requests) — no network, no SDK.
- Loop tests: happy path (2 tool calls → submit_finding → Finding returned,
  captures written in order); budget exhaustion at each cap (tool-call count,
  tokens, wall clock via fake clock) → forced submit with the exhaustion note;
  invalid finding → validation errors fed back, repaired on attempt 2; three
  invalid attempts → hard error; prose-only response → nudge then
  ToolChoice=required; MCP tool error → surfaced as tool_result, loop
  continues; ctx cancellation mid-loop → prompt exit, no orphaned captures.
- Schema tests: Finding fixtures (valid + one per constraint violation)
  validated against the checked-in schema; evidence-index → capture_ref
  resolution incl. the unresolvable-index error.

**orchestrator v0**
- Fake hound (canned Finding, controllable delay/failure). Phase progression
  writes checkpoints in order with correct statuses.
- Atomicity: torn-write simulation (inject rename failure) leaves prior state
  readable.
- Resume: run to completed-investigate, new orchestrator on same work dir →
  intake/investigate loaded not re-run (fake hound asserts zero extra calls),
  report runs, capture numbering continues.
- Budget/spend: checkpoint spend sums match middleware accounting; resumed
  spend counts toward caps.
- Phase timeout → failed checkpoint with deadline error; failed case resumes.

**CLI / e2e**
- `hunt --alert fixture.json` with scripted provider + fake Prometheus:
  exit 0, work dir complete, `report.json` matches golden.
- `hunt --resume` on the kill-mid-investigate work dir (the §4.3 acceptance
  test). Exit codes: bad alert file 2, pipeline-version mismatch on resume 2,
  phase failure 1.

**Live demo (manual, documented in the PR, not CI):** kind cluster + Prometheus
scraping it; break a pod manually (bad image or CPU-starved limits); craft the
alert JSON; run `hunt`; include the report in the PR description.

## 6. PR breakdown

Ordered; each merges green (`make check`) and scoped per CLAUDE.md. Estimates
are review-size, not effort.

| PR | Title | Contents | Depends on | Size |
|---|---|---|---|---|
| 1 | `spec: 002 M1 metrics path` | this document | — | S |
| 2 | `llm: tool-use surface + anthropic provider + middleware` | §3.1 types (breaking change to M0 stubs), anthropic impl, retry/accounting/capture decorators, `llmtest` scripted provider. New dep: `anthropic-sdk-go`. | 1 | L |
| 3 | `mcp-prom: server, guardrails, truncation` | §2 complete; unit/golden/conformance tests; README rewritten to document the tool surface + guardrail table. New dep: `modelcontextprotocol/go-sdk`. | 1 | L |
| 4 | `mcpclient: stdio session management` | `internal/mcpclient`: spawn server binary, session lifecycle, tool listing/calls, capture hook, per-call timeouts. | 3 | M |
| 5 | `hounds: metrics-hound loop + Finding v1` | §3.2–3.4; Finding schema as checked-in asset; `orchestrator.Finding` Go type evolved; loop + schema tests. | 2, 4 | M |
| 6 | `orchestrator: v0 state machine, checkpoints, resume` | §4 complete; minimal intake (file path) + terminal/JSON reporter; `hunt`/`hunt --resume` wired; e2e tests. | 5 | L |
| 7 | `m1: integration workflow + demo` | `test-integration` CI job, live-demo walkthrough doc, README M1 status, demo report artifact. | 3, 6 | S |

PRs 2 and 3 are independent after the spec merges and can proceed in parallel.
Each PR description notes which parts were agent-generated (spec 001 §7).

## 7. Open questions (defaults chosen, flag in review if wrong)

1. **Series ranking by `(max−min)`** biases toward large-magnitude series
   (§2.3). Accepting until evals say otherwise — normalized scoring is a
   one-constant change behind a golden-test update.
2. **`hunt --resume`** vs making `replay` do double duty: resume continues live
   from checkpoints; replay (M2) re-executes from LLM captures without paid
   calls. Distinct semantics → distinct flags, but naming is cheap to change
   now and expensive later.
3. **Finding lacks a `root_cause_class` field** in M1; the M4 grader will need
   one. Deliberately deferred: class taxonomy should be designed against the
   full scenario list, and adding a field to the schema is additive.
