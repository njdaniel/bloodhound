# bloodhound — Build Spec

**An AI incident-response agent pack for Kubernetes, written in Go.**
When an alert fires, bloodhound dispatches a pack of specialized investigator agents that query metrics, logs, deploys, and runbooks through MCP servers, argue about what they found, and deliver a root-cause hypothesis with evidence — scored against a reproducible benchmark of seeded failures.

- Repo: `github.com/njdaniel/bloodhound`
- License: MIT
- Status: spec v0.1 — 2026-07-28

---

## 1. Pitch (the interview version)

"On-call engineers waste the first 20 minutes of every incident gathering context. bloodhound automates that triage window: a planner agent fans out investigators over Prometheus, Loki, Kubernetes events, and your runbooks — each through an MCP server — then an adversarial verifier tries to tear the diagnosis apart before it's posted to Slack. It root-caused 17 of 20 seeded failure scenarios in the benchmark cluster at a median cost of ~$0.30 per incident, and the whole eval harness ships in the repo so you can reproduce the numbers yourself."

(The numbers are the deliverable. Everything in this spec exists to make that paragraph true.)

## 2. Goals and non-goals

**Goals**

1. Demonstrate production-grade multi-agent orchestration in Go: planner → parallel investigators → adversarial verifier → reporter, with budgets, timeouts, retries, and checkpointed state.
2. Demonstrate MCP fluency: every data source is an MCP server built with the official `modelcontextprotocol/go-sdk`; the orchestrator is an MCP client. Servers are independently usable and extractable into standalone repos later.
3. Demonstrate evals-first engineering: a seeded-failure benchmark with objective scoring (diagnosis accuracy, time-to-hypothesis, cost), run in CI on demand, results tracked over time.
4. Demonstrate AI-assisted development: the repo's own history shows spec-driven, agent-assisted building (specs/, CLAUDE.md, agent-authored PRs with human review).

**Non-goals (v1)**

- Not a general AIOps platform; no auto-remediation (report and recommend only — this is also the safety story).
- No web UI (CLI + Slack output only).
- No multi-cluster/fleet support.
- Not provider-exhaustive: Anthropic first-class; the provider interface exists but only one alternate backend (Ollama) is wired, untuned.

## 3. Architecture

```
                            ┌──────────────────────────────────────────────┐
 Alertmanager ──webhook──▶  │                 bloodhound                    │
 (or `bloodhound hunt`      │                                              │
  manual CLI trigger)       │  ┌────────┐   ┌──────────────────────────┐   │
                            │  │ Intake │──▶│        Planner            │  │
                            │  └────────┘   │  (case file + task plan)  │  │
                            │               └────────────┬─────────────┘   │
                            │            fan-out (parallel, budgeted)      │
                            │   ┌─────────┬──────────┬──────────┬──────┐   │
                            │   ▼         ▼          ▼          ▼      │   │
                            │ metrics   logs      changes    runbook   │   │
                            │ hound     hound     hound      hound     │   │
                            │   │         │          │          │      │   │
                            │   ▼         ▼          ▼          ▼      │   │
                            │  MCP:     MCP:       MCP:       MCP:     │   │
                            │  prom     loki       k8s        docs     │   │
                            │   └─────────┴────┬─────┴──────────┘      │   │
                            │                  ▼                       │   │
                            │        ┌──────────────────┐              │   │
                            │        │     Verifier      │ (adversarial│   │
                            │        │  (tries to refute)│  pass)      │   │
                            │        └────────┬─────────┘              │   │
                            │                 ▼                        │   │
                            │        ┌──────────────────┐              │   │
                            │        │     Reporter      │──▶ Slack /  │   │
                            │        └──────────────────┘    stdout /  │   │
                            │                                 JSON     │   │
                            └──────────────────────────────────────────────┘
```

### 3.1 Components

**Intake** (`internal/intake`) — HTTP server receiving Alertmanager webhooks; also a CLI path (`bloodhound hunt --alert alert.json`) for manual/replay runs. Normalizes alerts into a `Case` (ID, alert labels, firing time, cluster context). Dedupes by alert fingerprint within a window.

**Planner** (`internal/agents/planner`) — One LLM call. Input: the Case + a catalog of available investigators and their MCP tool descriptions. Output (structured JSON, schema-validated): an investigation plan — which hounds to dispatch, with per-hound focus questions and a token/cost budget split. Keeping the planner to a single structured call keeps it cheap and testable.

**Investigators ("hounds")** (`internal/agents/hounds`) — Parallel agentic loops, one goroutine each, hard-capped by per-hound budget (tokens, tool calls, wall clock). Each hound is an LLM tool-use loop whose tools are exactly one MCP server's tools:

| Hound | MCP server | Answers |
|---|---|---|
| metrics-hound | mcp-prom | What do the graphs say? When did it start? What correlates? |
| logs-hound | mcp-loki | What errors appear around onset? Which pods emit them? |
| changes-hound | mcp-k8s | What changed? Deploys, rollouts, OOMKills, node events, scaling. |
| runbook-hound | mcp-docs | Is there a known procedure/prior incident for this alert? |

Each hound returns a structured `Finding`: summary, confidence, evidence (queries run + raw excerpts), and dead ends (what it ruled out — dead ends are first-class output; they're what makes the final report trustworthy).

**Verifier** (`internal/agents/verifier`) — Adversarial pass. Input: the Case + all Findings. Instruction: construct the strongest argument that the leading hypothesis is wrong; may issue a bounded number of follow-up MCP queries to check its counter-theory. Output: verdict (confirmed / weakened / refuted), with reasoning. If refuted, the orchestrator runs one bounded re-plan cycle (max 1 in v1 — loop caps are explicit everywhere).

**Reporter** (`internal/agents/reporter`) — Renders the final incident brief: hypothesis + confidence, evidence table, timeline, what was ruled out, suggested next actions, cost/latency footer. Outputs: Slack Block Kit message, terminal (pretty), and JSON (for the eval harness).

**Orchestrator** (`internal/orchestrator`) — The state machine tying it together: `intake → plan → investigate (parallel) → verify → (replan?) → report`. Owns checkpointing (each phase's state persisted as JSON to a work dir — a crashed run resumes), budget accounting, per-phase timeouts, and OTel spans. This is deliberately hand-rolled rather than framework-based: the orchestrator is the exhibit.

### 3.2 MCP servers (`mcp/`)

All built on the official Go SDK (`github.com/modelcontextprotocol/go-sdk/mcp`, v1.7+). Each is a standalone binary speaking stdio transport (HTTP transport optional later), with its own README and tool docs — designed to be lifted into standalone repos once mature.

- **mcp-prom** — tools: `query_range`, `query_instant`, `list_alerts`, `series_metadata`. Guardrails: result-size caps, step clamping, query timeout.
- **mcp-loki** — tools: `query_logs` (LogQL), `label_values`, `tail_context` (window around a timestamp). Guardrails: line caps, mandatory time bounds.
- **mcp-k8s** — read-only: `recent_events`, `rollout_history`, `pod_status`, `node_pressure`, `recent_restarts`. Uses `client-go` with a read-only ServiceAccount; the RBAC manifest ships in `deploy/`.
- **mcp-docs** — tools: `search_runbooks`, `get_runbook`, `similar_incidents`. v1 backend: local markdown directory + SQLite FTS5 (no vector DB dependency; keyword search is honest and debuggable — a documented tradeoff).

Design rule stated in every server README: tools return **bounded, model-shaped** data (summaries + capped excerpts), never raw dumps. Tool descriptions are prompt engineering.

### 3.3 LLM provider layer (`internal/llm`)

```go
type Provider interface {
    // Complete runs one model turn. Tool use is expressed via the request's
    // tool definitions; the response either finishes or requests tool calls.
    Complete(ctx context.Context, req Request) (Response, error)
    CountCost(usage Usage) Cost
    Name() string
}
```

- `internal/llm/anthropic` — first-class, via official `anthropic-sdk-go`. Model tiers from config: planner/verifier on a stronger model, hounds on a faster one (a documented cost/quality tradeoff, measured in the evals).
- `internal/llm/ollama` — exists to prove the interface; not tuned; benchmark table gets an asterisked local-model row.
- Middleware wrappers (decorator pattern): retry w/ backoff, token accounting, OTel spans, request/response capture for replay.

### 3.4 Observability

- OTel tracing end-to-end: one trace per case; spans for phases, LLM calls, MCP tool calls (attributes: tokens, cost, tool name, truncation applied). Demo docker-compose ships Jaeger.
- `bloodhound cost <case-id>` prints the money/latency breakdown per phase/agent.
- Every LLM request/response captured to the case work dir (scrub rules for secrets) — this is also the eval replay substrate.

## 4. Eval harness (`evals/`)

The centerpiece. Everything reproducible from a clean machine: `make eval` → kind cluster + observability stack + scenario injection + scored run.

### 4.1 Bench cluster

`evals/bench/` — kind config + manifests: Prometheus, Loki, Alertmanager (webhook → bloodhound), plus a small demo microservice app (3-4 services, e.g. a queue-backed order pipeline) instrumented with metrics/logs. One command up (`make bench-up`), one command down.

### 4.2 Scenarios

A scenario = YAML manifest + injector + ground truth:

```yaml
id: S07-oom-after-deploy
title: Deployment raises memory limit victim's neighbor OOMKills
inject: ./inject.sh          # applies bad manifest / breaks the thing
alert: HighErrorRate          # alert expected to fire
ground_truth:
  root_cause_class: resource.oom
  culprit_kind: Deployment
  culprit_name: checkout
  causal_chain: [deploy, memory-pressure, oomkill, errors]
scoring:
  must_mention: [OOMKilled, checkout]
  red_herrings: [network, dns]   # penalize if blamed
timeout: 10m
```

**v1 scenario list (18):**

| # | Class | Scenario |
|---|---|---|
| S01 | deploy | Bad image tag → CrashLoopBackOff |
| S02 | deploy | New version regresses latency (no crash) |
| S03 | deploy | Missing ConfigMap key after rollout |
| S04 | resource | OOMKill under load |
| S05 | resource | CPU throttling → tail latency |
| S06 | resource | Disk pressure evicts pods |
| S07 | resource | Memory-limit change OOMs a neighbor |
| S08 | config | Wrong env var (connects to wrong DB) |
| S09 | config | Expired TLS cert between services |
| S10 | config | Bad HPA settings → flapping replicas |
| S11 | dependency | Downstream service down → cascading 5xx |
| S12 | dependency | Slow DB (lock contention) → queue backup |
| S13 | dependency | External API rate-limits the app |
| S14 | network | NetworkPolicy blocks a needed port |
| S15 | network | DNS failures (CoreDNS scaled to 0) |
| S16 | node | Node NotReady, pods pending |
| S17 | compound | Deploy + coincidental node event (red herring) |
| S18 | control | Alert fires, nothing actually wrong (flaky alert) — correct answer is "no defect found" |

S17/S18 are the differentiators: they measure whether the system resists plausible-but-wrong stories. That's the verifier's job, and the ablation (§4.4) proves it.

### 4.3 Scoring

Grader (`evals/grader`) is deterministic Go, not an LLM, wherever possible:

- **Root-cause accuracy**: predicted `root_cause_class` + culprit vs ground truth (exact class match; culprit fuzzy-matched on kind+name). Primary metric: correct@1.
- **Evidence honesty**: every claim in the report must cite a captured tool result (checked against the request/response log). Uncited claims → penalty. (An LLM-assisted checker maps claims→evidence, but the evidence existence check is mechanical.)
- **Red-herring resistance**: penalty for blaming listed red herrings; S18 scored on saying "nothing found."
- **Ops metrics**: wall-clock to final report, total tokens, cost.

Output: `evals/results/<run-id>/report.md` + JSON; a small script renders the README badge table. CI runs a 4-scenario smoke suite on PRs (behind a label, needs API key secret); the full 18 run on demand/nightly.

### 4.4 Ablations (the "understands why" evidence)

One `make` target each, results in the README:

1. No verifier (does adversarial review actually reduce wrong-confident answers? measure on S17/S18).
2. Single mega-agent vs planner+hounds (same budget — does structure beat scale?).
3. Model-tier mix (all-fast vs mixed vs all-strong — cost/accuracy frontier).

## 5. Repo layout

```
bloodhound/
├── cmd/bloodhound/            # CLI: serve, hunt, replay, cost, eval
├── internal/
│   ├── intake/
│   ├── orchestrator/          # state machine, checkpoints, budgets
│   ├── agents/                # planner, hounds, verifier, reporter
│   ├── llm/                   # provider iface, anthropic, ollama, middleware
│   ├── mcpclient/             # MCP client pool/session mgmt
│   └── report/                # slack blocks, terminal, json renderers
├── mcp/
│   ├── prom/  ├── loki/  ├── k8s/  └── docs/     # each: main.go + README
├── evals/
│   ├── bench/                 # kind + manifests + demo app
│   ├── scenarios/S01..S18/
│   ├── grader/
│   └── results/
├── specs/                     # numbered design docs (this file = 001)
├── deploy/                    # RBAC, docker-compose (jaeger), helm later
├── .github/workflows/         # ci.yml, eval-smoke.yml
├── CLAUDE.md
└── README.md
```

## 6. Milestones

Each milestone is demoable and merges via PR with the AI-assisted workflow (§7). Rough calendar assumes nights-and-weekends pace.

- **M0 — Skeleton (week 1):** repo scaffold, CI (build/vet/test), CLI stub, provider interface + anthropic impl with middleware, `specs/` seeded. Demo: `bloodhound hunt --alert fixture.json` runs a single hardcoded LLM call and prints a fake report.
- **M1 — First MCP server + one hound (weeks 2-3):** mcp-prom complete w/ tests against a Prometheus container; metrics-hound tool-use loop; orchestrator v0 (intake→one hound→report). Demo: real diagnosis of a manually broken pod, metrics only.
- **M2 — The pack (weeks 4-6):** mcp-loki, mcp-k8s, mcp-docs; planner; parallel hounds with budgets; checkpointing; OTel. Demo: full fan-out on a live alert; Jaeger trace screenshot in README.
- **M3 — Verifier + reporter (week 7):** adversarial pass, replan cycle, Slack output. Demo: verifier catching a planted red herring.
- **M4 — Eval harness (weeks 8-10):** bench cluster, all 18 scenarios, grader, results pipeline, CI smoke suite. Demo: `make eval` produces the scoreboard. **This milestone is the portfolio moment — README gets the numbers.**
- **M5 — Ablations + polish (weeks 11-12):** three ablations, cost tuning, README case study ("anatomy of one incident," trace + report walkthrough), demo GIF/asciinema, blog post draft.

Cut-scope order if time-boxed: drop ablations 2-3 → drop Ollama backend → cut scenarios to 12 (keep S17/S18) → drop Slack (JSON/terminal only). Never cut: verifier, grader, checkpointing.

## 7. AI-assisted development workflow (portfolio pillar 4)

The repo's history is itself an exhibit:

- **Spec-driven:** every feature starts as `specs/NNN-*.md` (problem, design, tradeoffs, test plan) — reviewed before code.
- **CLAUDE.md** encodes house rules: architecture map, code conventions, "always run `make check` before proposing a diff," how to run scoped tests, PR etiquette.
- **Agent-authored PRs:** implementation PRs driven by Claude Code from the spec; Nick reviews and requests changes in comments (visible history of human oversight). PR descriptions note which parts were agent-generated.
- **CI review agent:** lightweight workflow that runs an agent review pass on PRs (advisory comment, never blocking).
- **README section "How this repo is built"** documents the loop with links to example PRs. This section is what turns the meta-story into something a hiring manager can verify in two clicks.

## 8. Key tradeoffs (documented up front, revisited in specs)

- **Hand-rolled orchestrator vs framework:** the orchestrator is the thing being showcased; frameworks would hide it. Cost: more code to get budgets/checkpoints right. (If asked "why not LangChainGo/ADK" — this is the answer, written down on day one.)
- **Deterministic grader vs LLM judge:** mechanical scoring is reproducible and cheap; costs flexibility in phrasing-matching. LLM assists only the claim→evidence mapping, never the verdict.
- **Keyword runbook search vs embeddings:** SQLite FTS5 keeps v1 dependency-free and debuggable; embeddings are a listed v2 upgrade with a benchmark to justify it.
- **Report-only, no remediation:** safety and scope; also makes the demo runnable against real clusters without fear.
- **stdio MCP transport in v1:** simplest ops story (orchestrator spawns servers); HTTP+auth is the v2 path using the SDK's auth package.

## 9. README skeleton

```markdown
# bloodhound 🐕
AI incident-response agent pack for Kubernetes, in Go.
Alert fires → agents investigate metrics/logs/changes/runbooks via MCP →
adversarial verifier stress-tests the diagnosis → evidence-backed brief in Slack.

[benchmark badge: 17/20 scenarios | median $0.31/incident | p50 3m42s]

## Why
[3 sentences: the first 20 minutes of every incident are context-gathering. Automate that.]

## How it works
[architecture diagram + one paragraph per phase]

## Benchmark
[scoreboard table by scenario class + ablation table + "reproduce: make eval"]

## Anatomy of one incident
[walkthrough: alert → plan → findings (incl. a dead end) → verifier pushback → final report, with Jaeger trace screenshot]

## Run it
[quickstart: make bench-up && make demo — one seeded scenario end-to-end]

## MCP servers
[table of the four servers + note each is usable standalone]

## How this repo is built
[the AI-assisted workflow, links to example specs and agent PRs]

## Design docs
[links into specs/]
```

## 10. Success criteria

1. `make eval` on a clean machine reproduces the README scoreboard.
2. ≥80% correct@1 on the 18 scenarios with the mixed-tier config; S18 (control) answered correctly.
3. Verifier ablation shows a measurable drop on S17/S18 without it (the "structure matters" proof).
4. A stranger can go alert→report on their own cluster in <30 minutes with the quickstart.
5. Repo history shows ≥5 spec→agent-PR→human-review cycles.
