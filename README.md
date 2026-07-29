# bloodhound 🐕

**AI incident-response agent pack for Kubernetes, written in Go.**

When an alert fires, bloodhound dispatches a pack of specialized investigator
agents that query metrics, logs, deploys, and runbooks through MCP servers,
argue about what they found, and deliver an evidence-backed root-cause brief
to Slack — scored against a reproducible benchmark of seeded failures.

> **Status: M1 — the metrics path works end to end.** An alert goes in, a
> checkpointed investigation queries a live Prometheus through an MCP server,
> and a schema-valid, evidence-cited report comes out. One hound, one data
> source; the rest of the pack is M2. The design is complete — see
> [specs/001-build-spec.md](specs/001-build-spec.md) and
> [specs/002-m1-metrics-path.md](specs/002-m1-metrics-path.md).
>
> **There are no benchmark numbers here yet, deliberately.** Accuracy, cost,
> and latency are measured by the eval harness in M4, and quoting anything
> before then would be inventing it.

## Why

On-call engineers lose the first 20 minutes of every incident to context
gathering: pulling up dashboards, grepping logs, checking what deployed.
bloodhound automates that triage window. It does **not** auto-remediate —
it reports, with every claim backed by a captured query result.

## How it works

```
Alertmanager ──▶ Intake ──▶ Planner ──▶ ┌ metrics-hound ─ MCP: Prometheus
                                        ├ logs-hound ──── MCP: Loki
                                        ├ changes-hound ─ MCP: Kubernetes
                                        └ runbook-hound ─ MCP: docs
                                              │
                                        Verifier (adversarial: tries to
                                        refute the leading hypothesis)
                                              │
                                        Reporter ──▶ Slack / terminal / JSON
```

The orchestrator is a hand-rolled state machine with explicit budgets
(tokens, tool calls, wall clock), phase checkpointing, and OpenTelemetry
tracing end to end. The four data-source servers are standalone MCP servers
built on the official [Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## What works today (M1)

The metrics path is complete. Everything else in the diagram above is a
milestone away.

```sh
go build -o bin/bloodhound ./cmd/bloodhound
go build -o bin/mcp-prom   ./mcp/prom
export ANTHROPIC_API_KEY=sk-ant-…

bloodhound hunt --alert alert.json --prom-url http://localhost:9090
bloodhound cost <case-id>
```

| Command | What it does |
|---|---|
| `hunt --alert <file>` | Investigate one Alertmanager-format alert: intake → metrics-hound → report. Writes a case work dir with checkpoints, every LLM and MCP call captured, `report.json` and `report.txt`. |
| `hunt --resume <case-id>` | Continue a case from its last completed checkpoint. Completed phases are loaded, never re-run, and spend already recorded counts toward the budget — a crash loop cannot multiply the bill. |
| `cost <case-id>` | Per-phase and total tokens, dollars, tool calls, and wall clock, summed from the checkpoints. |

Budgets are explicit and default to 12 tool calls / 50k tokens / 3m wall
clock. The hound terminates by calling a synthetic `submit_finding` tool, so
its output is schema-checked structured data rather than parsed prose, and
every evidence entry carries a `capture_ref` the loop assigns — the model
cites tool calls by index and cannot invent a citation.

**[mcp-prom](mcp/prom/README.md)** is a standalone MCP server (stdio, official
Go SDK) usable by any MCP client. Its four tools return bounded, model-shaped
data — summaries plus capped excerpts, with every truncation marked in the
payload:

| Tool | Returns |
|---|---|
| `query_range` | Up to 15 series over a ≤24h window, ≤120 points each, with min/max/avg/last computed at full resolution. |
| `query_instant` | Up to 100 samples at one instant, outliers first. |
| `list_alerts` | Up to 50 active alerts, newest first. |
| `series_metadata` | Up to 25 metrics with type, help, and sample label values, from the last 1h of series — metric discovery before writing PromQL. |

**Not yet:** logs, Kubernetes, and runbook hounds (M2), the adversarial
verifier and Slack output (M3), the eval harness and any numbers (M4).

## Demo

[**docs/demo-m1.md**](docs/demo-m1.md) walks the whole path: break a pod in a
`kind` cluster, scrape it, hand the alert to `bloodhound hunt`, read the
report, price the case. It is explicit about which steps were executed and
which need an API key — the committed
[case artifact](docs/demo-m1/case) ran against a real Prometheus with a
scripted model standing in for Anthropic, and says so.

The Prometheus half reproduces with Docker alone, no key:

```sh
make test-integration
```

## Benchmark

Coming with M4: 18 seeded failure scenarios (bad deploys, OOMKills, cert
expiry, red herrings, and a nothing-is-actually-wrong control) run in a
`kind` cluster, scored by a deterministic grader on root-cause accuracy,
evidence honesty, time-to-hypothesis, and cost. Reproducible with `make eval`.

## Roadmap

| Milestone | Deliverable |
|---|---|
| M0 ✅ | Skeleton: CLI, provider interface, CI, specs |
| M1 ✅ | mcp-prom + metrics-hound + orchestrator v0: the metrics path end to end |
| M2 | Full pack: all four MCP servers, planner, parallel hounds, tracing |
| M3 | Adversarial verifier + Slack reporter |
| M4 | Eval harness: 18 scenarios, grader, scoreboard in this README |
| M5 | Ablations (verifier off, mono-agent, model tiers) + case study |

## How this repo is built

bloodhound is developed spec-first with AI-assisted workflows: every feature
starts as a design doc in [specs/](specs/), implementation PRs are driven by
coding agents from those specs and reviewed by a human, and CI includes an
advisory agent review pass. [CLAUDE.md](CLAUDE.md) encodes the house rules.
This workflow is part of what the project demonstrates.

A house rule worth stating out loud, because it constrains what this README is
allowed to say: **claims need captured evidence behind them.** Findings cite
tool results; the demo doc marks its unexecuted steps as unexecuted; the
scoreboard stays empty until the grader fills it in.

## License

MIT
