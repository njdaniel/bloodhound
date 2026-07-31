# mcp-prom

Standalone MCP server (stdio transport) exposing Prometheus to bloodhound's
metrics-hound — and to any MCP client. Built on the official
`github.com/modelcontextprotocol/go-sdk/mcp`. Talks to the Prometheus HTTP API
directly via `net/http` + `encoding/json`; no `client_golang` dependency
(stdlib-first rule — the query API is small and stable; revisit only if tests
ever need remote-write).

Spec: `specs/002-m1-metrics-path.md` §2 is the authoritative design.

## Design rule: bounded, model-shaped data

Tools return **bounded, model-shaped** data — capped result sets, downsampled
series, summaries plus capped excerpts — never raw dumps. Every truncation is
**marked in the payload** (the `truncation` block), so the model can never
mistake a capped result for a complete one. Tool descriptions are prompt
engineering: they are written for a model and state the guardrails up front
("range limited to 24h; narrow your window").

Errors the model can act on (bad PromQL, too-wide window) come back as MCP
**tool errors** with an actionable message, not protocol failures.

## Running

```sh
go build -o mcp-prom ./mcp/prom
PROM_URL=http://localhost:9090 ./mcp-prom
```

Configuration (flags override environment):

| Setting | Flag | Default | Meaning |
|---|---|---|---|
| `PROM_URL` | `-prom-url` | — (required) | Prometheus base URL |
| `PROM_TIMEOUT` | `-prom-timeout` | `15s` | upstream query timeout |

No auth in v1 (the bench cluster is local).

## Guardrails

One table, defined once in `guardrails.go`, shared by all tools:

| Constant | Value | Applied to |
|---|---|---|
| `QueryTimeout` | 15s | every upstream HTTP call (context deadline) |
| `MaxRange` | 24h | `query_range` end−start; violations are an **error, not a clamp** — the model is told to narrow the window |
| `MaxPointsPerSeries` | 120 | step clamping |
| `MaxSeries` | 15 | `query_range` result cap |
| `MaxInstantSamples` | 100 | `query_instant` result cap |
| `MaxAlerts` | 50 | `list_alerts` |
| `MaxMetadataMetrics` | 25 | `series_metadata` |
| `MaxLabelValues` | 10 | per label key in `series_metadata` |
| `MetadataLookback` | 1h | `series_metadata` discovery window (`start`/`end` on `/api/v1/series`) |
| `MaxUpstreamSeries` | 2000 | `limit` sent to `/api/v1/series` by `series_metadata` |
| `MaxUpstreamMetadata` | 2000 | `limit` sent to `/api/v1/metadata` by `series_metadata` |
| `MaxStringLen` | 120 | any label value / metadata string (truncate to 117 + `…`) |
| `MaxAlertAnnotationLen` | 200 | alert annotation values |
| `MaxQueryAnnotations` | 5 | PromQL annotations per severity in `query_range` / `query_instant` |
| `MaxQueryAnnotationLen` | 200 | one PromQL annotation (real ones run 132–141 bytes, with the metric name and position at the end) |
| `MaxResponseBytes` | 32 KiB | serialized tool result; triggers point-thinning |
| `FloatFormat` | `%.6g` | every sample value |

### Exact truncation strategy (`query_range`)

Applied in this order; each step is deterministic (golden tests lock the
serialized results byte-exact):

1. **Range check.** `end − start > 24h` → tool error; never a silent clamp.
2. **Step clamp.** `effective_step = max(step_seconds, ceil((end−start)/120))`;
   the same ceiling is the default when `step_seconds` is omitted. Echoed back
   as `resolved_step_seconds`.
3. **Query** with the timeout.
4. **Series cap.** More than 15 series → rank by `(max − min)` descending
   (volatility is what an investigator wants), tie-break by max `|value|`
   descending, then lexicographically by sorted labelset. Keep the top 15;
   counts and the ranking rule land in `truncation.note`.
5. **Value formatting.** Timestamps as unix seconds; values as strings via
   `%.6g` (strings dodge float JSON round-trip noise).
6. **Size backstop.** Serialized result over 32 KiB → thin points: keep first
   and last, drop every second interior point, repeat until it fits. Sets
   `points_thinned: true`. Per-series `stats` are always computed from the
   **full-resolution** data, before thinning. Thinning bottoms out at two
   points per series — first and last; a result still over the cap there
   (labels and series count, which this step cannot thin) is returned
   oversized and says so in `truncation.note`.

### Upstream PromQL annotations

Prometheus attaches non-fatal annotations to a successful evaluation, in two
arrays at two severities. Measured against v3.5.0:

```
warnings: PromQL warning: bucket label "le" is missing or has a malformed
          value of "" for metric name "…" (1:25)
infos:    PromQL info: metric might not be a counter, name does not end in
          _total/_sum/_count/_bucket: "…" (1:6)
```

A warning means the numbers in the payload may be meaningless — a
`histogram_quantile` over series with malformed `le` labels is a routine
incident-response move that returns a number Prometheus knows nothing about. An
info means the expression is a likely mistake though the result is well-defined;
`rate()` over a non-counter is the one an LLM writing PromQL hits most.

`query_range` and `query_instant` carry both to the model in an `annotations`
block, which is **omitted entirely when Prometheus raised nothing**, so its
presence is the signal:

```json
"annotations": {
  "warnings": ["PromQL warning: …"],
  "warnings_total": 8,
  "infos": ["PromQL info: …"],
  "infos_total": 1,
  "note": "3 further warnings dropped; kept one of each distinct kind first, then alphabetically. …"
}
```

It sits beside `truncation` rather than inside it: `truncation` answers "how
much of the answer am I seeing", an annotation answers "is this an answer at
all", and nothing was dropped when Prometheus reports a malformed bucket label.
The severities stay in separate arrays because they call for different actions.

Annotations are passed **verbatim** — the metric name and `(line:col)` position
are the only parts that say which query to fix, so there is nothing worth
classifying. That is the other half of the `series_metadata` rule below:
classify a warning describing a cap bloodhound asked for, pass everything else
through. These tools send no `limit`, so nothing here is about a bloodhound cap.

Bounded like everything else: at most `MaxQueryAnnotations` per severity, each
capped at `MaxQueryAnnotationLen`, with the drop marked in `note`.

Which ones survive the cap takes two steps, and both are load-bearing.

**Ordering the input**, because Prometheus accumulates annotations in a map and
serializes them in map iteration order — identical calls to one server return
the same annotations differently ordered, which the integration test measures
and logs on every run. Keeping "the first five" of an arbitrary order would hand
the model a different subset every call.

**Then taking one of each distinct kind before taking a second of any**, because
plain alphabetical order is actively biased here rather than merely arbitrary.
Prometheus raises the per-metric annotations once per affected metric and their
template starts with `bucket label`, while essentially every other template
starts later in the alphabet (`encountered a mix…`, `invalid quantile…`,
`quantile value should be…`, `vector contains…`). So the near-identical repeats
sort first *every time*, and an alphabetical cap drops the one annotation that
says something new. Measured: `histogram_quantile(1.5, {job="…"})` returns nine
warnings — eight `bucket label` repeats and one `quantile value should be
between 0 and 1, got 1.5` — and the five alphabetically first are all
`bucket label`. A model told only those would fix the metric type and repeat the
1.5-should-be-0.95 mistake on its next query. Annotations are grouped by the
message up to its first quoted operand or number (the template), so while there
are no more kinds than slots every kind is represented; repeats take the
leftovers. The kept set is sorted for presentation, and is a function of the
*set* of annotations, never of upstream order.

`list_alerts` and `series_metadata` get no such block: annotations come from
PromQL evaluation, and `/api/v1/alerts` and `/api/v1/metadata` do not evaluate
an expression. Neither response carries a `warnings` or `infos` key at all
against v3.5.0, and the integration suite pins that, so the scope decision fails
loudly rather than silently if it stops holding.

## Tool surface

All results are one MCP text content block containing JSON, always with a
`truncation` block. `query_range` and `query_instant` additionally carry an
`annotations` block when Prometheus raised any (see above).

### `query_range`

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
  }
}
```

`stats` exists so the model can reason about a series without reading every
point — often it never needs to.

### `query_instant`

Input schema:

```json
{
  "type": "object",
  "required": ["query"],
  "additionalProperties": false,
  "properties": {
    "query": { "type": "string", "description": "PromQL expression" },
    "time": { "type": "string", "format": "date-time", "description": "RFC 3339 evaluation instant; defaults to now" }
  }
}
```

Output: `{ "result_type": "vector"|"scalar", "samples": [{ "labels": {...},
"value": "1.5", "timestamp": 1753700000 }], "truncation": {...} }`. At most
100 samples, ranked by `|value|` descending (you almost always want the
outliers), deterministic tie-break on the sorted labelset.

### `list_alerts`

Input schema:

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "state": { "type": "string", "enum": ["firing", "pending", "all"],
      "description": "Alert state filter; defaults to \"firing\"" }
  }
}
```

Output: `{ "alerts": [{ "name", "state", "active_at", "value", "labels": {...},
"annotations": {...} }], "truncation": {...} }`. Sorted by `active_at`
descending, capped at 50; annotation values truncated to 200 characters.

### `series_metadata`

Input schema:

```json
{
  "type": "object",
  "required": ["match"],
  "additionalProperties": false,
  "properties": {
    "match": { "type": "string",
      "description": "Series selector, e.g. '{namespace=\"shop\"}', or a bare metric name" }
  }
}
```

Output: `{ "metrics": [{ "name", "type", "help", "labels": { "<key>":
["sample", "values"] } }], "truncation": {...} }`. Backed by `/api/v1/series`
(what matches) plus `/api/v1/metadata` (type/help). At most 25 metrics,
alphabetical (discovery tool — determinism beats relevance ranking), with up
to 10 sample values per label key.

Both upstream calls are bounded as well as the output: the series lookup is
restricted to the last `MetadataLookback` and to `MaxUpstreamSeries` series,
and the metadata lookup to `MaxUpstreamMetadata` metrics. So discovery only
sees series with recent samples — a metric absent from the result may still
exist with older data, which the tool description tells the model. `limit` is
ignored by Prometheus versions that predate it; the client-side caps are what
guarantee a bounded result.

Both upstream caps are marked in `truncation.note` when they bite. The series
cap is detected from a *truncation* warning in the response, not from the
result count, so a server that ignores `limit` is never reported as having
truncated — and warnings that mean something else (a Thanos/Cortex partial
response, a failing `remote_read`) are passed through verbatim rather than
blamed on the limit. The metadata cap has no warning to read, so a full
metadata map is reported as possibly truncated, but only when a returned
metric is actually missing its `type` or `help`: that empty field may then
mean "not fetched" rather than "not registered".

## Tests

- Unit tests against an `httptest` fake Prometheus: step-clamp arithmetic,
  ranking tie-breaks, size backstop, error paths.
- Golden tests: full serialized tool results, byte-exact
  (`go test ./mcp/prom/ -run TestGolden -update` to regenerate).
- Stdio conformance: the built binary is spawned by the go-sdk client; tool
  names and input schemas asserted, each tool called once.
- Integration (`-tags integration`, `make test-integration`): the built binary
  is driven over stdio against a real Prometheus container scraping a
  test-owned `/metrics` endpoint, to catch wire-format drift the fake cannot.
  CI runs it in its own job on every PR — no API key, no paid calls. The build
  tag keeps `make check`'s build, vet and test steps from touching these files,
  which is what keeps it to seconds; `make lint` does still analyse them, since
  `.golangci.yml` sets `build-tags: [integration]`. Without docker they skip
  rather than fail.
- One integration test scrapes a fixture registering three times the larger of
  `MaxUpstreamSeries` and `MaxUpstreamMetadata` metric names, so both upstream
  limits actually fire and the wording of the real truncation warning is
  asserted rather than assumed.
- One integration test provokes real PromQL annotations from the S01 fixture —
  `histogram_quantile` over a gauge for a warning, `rate()` over a gauge for an
  info — and asserts the exact wording the server emits, that the two severities
  arrive in separate arrays, that `/api/v1/alerts` and `/api/v1/metadata` emit
  none, and that repeated identical calls keep the *same* capped subset (which
  they do not without the ordering, since upstream order is map iteration
  order). A `mixed kinds` subtest fires `histogram_quantile(1.5, …)` so the
  server really does raise eight per-metric repeats plus one distinct warning,
  and checks the distinct one survives the cap. The wording constants are shared
  with the unit tests, so those fixtures cannot drift from what the wire
  carries.
- The verbatim pass-through of a *non-truncation* warning in `series_metadata`
  is unit-test-only: Prometheus v3.5.0 emits no such warning on
  `/api/v1/series`, so provoking it needs a fanout backend (Thanos, Cortex) this
  suite does not run. A failing `remote_read` does warn, but only on
  `/api/v1/query` — not on `/api/v1/series`, which is the only endpoint
  `series_metadata` reads warnings from. Since `query_instant` and `query_range`
  now surface annotations, that `remote_read` warning does reach the model, just
  through those tools rather than through `series_metadata`.
