# CLAUDE.md — house rules for agents working in this repo

## What this project is

bloodhound is an AI incident-response agent pack for Kubernetes, in Go.
Read `specs/001-build-spec.md` before making any non-trivial change — it is
the source of truth for architecture and scope. Current milestone: M0→M1.

## Architecture map

- `cmd/bloodhound/` — CLI entrypoint (serve, hunt, replay, cost).
- `internal/orchestrator/` — the state machine: intake → plan → investigate →
  verify → report. Budgets and checkpoints live here. Hand-rolled on purpose;
  do not introduce an agent framework.
- `internal/agents/` — planner, hounds, verifier, reporter.
- `internal/llm/` — provider interface. Anthropic is first-class
  (`anthropic-sdk-go`); everything model-related goes through `llm.Provider`.
- `mcp/*` — standalone MCP server binaries (official `go-sdk`). Tools must
  return bounded, model-shaped data: summaries + capped excerpts, never raw dumps.
- `evals/` — bench cluster, scenarios, deterministic grader.

## Conventions

- Go 1.24+. Standard library first; justify every new dependency in the PR.
- Errors: wrap with `fmt.Errorf("doing x: %w", err)`; no panics outside main.
- Every exported symbol gets a doc comment. Packages get a package comment.
- Structured outputs from agents are JSON with schemas checked in tests.
- All LLM and MCP calls take a `context.Context` and respect budgets.

## Workflow

- Feature work starts from a spec in `specs/NNN-*.md`. If there is no spec,
  write one first and stop for review.
- Run `make check` (fmt, vet, build, test) before proposing any diff.
- Keep PRs scoped to one milestone item. Note in the PR description which
  parts were agent-generated.
- Never commit secrets. API keys come from env (`ANTHROPIC_API_KEY`).
- Eval runs that call paid APIs are opt-in only (label-gated in CI).
