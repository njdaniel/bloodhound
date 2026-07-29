#!/usr/bin/env bash
# One-time label setup for the /sprint workflow. Idempotent.
set -euo pipefail

create() { gh label create "$1" --color "$2" --description "$3" --force; }

# Areas — adjust to taste per repo.
create "area/orchestrator" "1D76DB" "Orchestrator, state machine, budgets"
create "area/agents"       "5319E7" "Planner, hounds, verifier, reporter"
create "area/mcp"          "0E8A16" "MCP servers and client plumbing"
create "area/llm"          "006B75" "Provider layer, middleware, cost accounting"
create "area/evals"        "D93F0B" "Bench cluster, scenarios, grader"
create "area/infra"        "C5DEF5" "CI, build, deploy, tooling"
create "area/docs"         "FEF2C0" "Specs, README, runbooks"

# Status — Kanban state. Exactly ONE per open issue; closed issues are "Done".
create "status/backlog"     "EEEEEE" "Not committed to a sprint"
create "status/ready"       "0E8A16" "In sprint, unblocked, criteria written"
create "status/blocked"     "B60205" "Waiting on a dependency (see issue body)"
create "status/in-progress" "1D76DB" "Worker dispatched"
create "status/in-review"   "FBCA04" "PR open, awaiting human merge"

# Priority
create "prio/p1" "B60205" "Sprint-blocking"
create "prio/p2" "FBCA04" "Should land this sprint"
create "prio/p3" "C2E0C6" "Nice to have"

# Size
create "size/S" "EDEDED" "Under half a day"
create "size/M" "D4C5F9" "Half a day to two days"
create "size/L" "F9D0C4" "Multi-day — consider splitting"

echo "Labels ready."
