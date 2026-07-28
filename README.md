# bloodhound 🐕

**AI incident-response agent pack for Kubernetes, written in Go.**

When an alert fires, bloodhound dispatches a pack of specialized investigator
agents that query metrics, logs, deploys, and runbooks through MCP servers,
argue about what they found, and deliver an evidence-backed root-cause brief
to Slack — scored against a reproducible benchmark of seeded failures.

> **Status: early development (M0 skeleton).** The design is complete — see
> [specs/001-build-spec.md](specs/001-build-spec.md). Benchmark numbers will
> appear here when the eval harness lands (M4).

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

## Benchmark

Coming with M4: 18 seeded failure scenarios (bad deploys, OOMKills, cert
expiry, red herrings, and a nothing-is-actually-wrong control) run in a
`kind` cluster, scored by a deterministic grader on root-cause accuracy,
evidence honesty, time-to-hypothesis, and cost. Reproducible with `make eval`.

## Roadmap

| Milestone | Deliverable |
|---|---|
| M0 ✅ | Skeleton: CLI, provider interface, CI, specs |
| M1 | mcp-prom + metrics-hound: first real diagnosis |
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

## License

MIT
