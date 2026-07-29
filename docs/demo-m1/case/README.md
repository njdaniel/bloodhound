# Case artifact — what in here is real

This directory is a complete `bloodhound` case work dir (spec 002 §4.2),
copied verbatim from a run performed on 2026-07-29. It is committed as
evidence for [docs/demo-m1.md](../../demo-m1.md) §4b.

**Real:**

- The CLI, orchestrator, checkpointing, capture numbering, reporter, and cost
  accounting — every line of production code on the M1 path ran.
- `mcp-prom`, spawned as a real binary and driven over stdio.
- Prometheus: a real `prom/prometheus:v3.5.0` container, scraping a real
  `/metrics` endpoint every 250ms. Every sample under `captures/mcp/` was
  scraped, stored, queried, and shaped by that server. The `job` and
  `instance` labels on the series are the proof — Prometheus attaches those to
  scraped data; the exposition never emits them.

**Not real:**

- **The model.** A scripted `llmtest.Provider` stood in for Anthropic. The
  three request/response pairs under `captures/llm/` are canned: the requests
  are genuine (real system prompt, real tool definitions fetched from the live
  MCP session, real tool results), but the assistant turns were written by a
  test, not generated. No paid API call was made by this run.
- **The token counts.** `7100 in / 620 out` are the scripted provider's canned
  `Usage` values. They travelled through the real accounting middleware, so
  the plumbing is exercised — but they measure nothing.
- **`usd: 0`** is, however, accurate: nothing was spent.
- **The workload.** These metrics describe `checkout-7d9f`, a fixture the test
  process exposes (`internal/promtest`), not a pod in a cluster. A real
  kind cluster with a real `CrashLoopBackOff` pod *was* brought up and scraped
  — see demo-m1.md §1–§3 — but the committed case here is the reproducible
  one, because anyone can regenerate it with Docker alone.

## Regenerating it

```sh
BLOODHOUND_DEMO_WORK=./work \
  go test -tags integration -run TestHuntAgainstRealPrometheus ./cmd/bloodhound/
```

The case ID, timestamps, ports, and sample values differ every run; the shape
does not.
