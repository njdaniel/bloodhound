# /sprint workflow

Drop-in goal-oriented sprint workflow for Claude Code. Plans a sprint from
your specs/roadmap, creates the GitHub issues, triages the backlog, and runs
retros — always proposing before acting, with all GitHub writes via `gh`.

## Install

Copy into the target repo:

```bash
cp -r .claude/commands <repo>/.claude/commands   # merge if it exists
cp scripts/sprint-labels.sh <repo>/scripts/
chmod +x <repo>/scripts/sprint-labels.sh
```

Requires the `gh` CLI authenticated (`gh auth status`).

## Use

Inside the repo, in Claude Code:

```
/sprint            # plan the next sprint (proposes goal + issues, then creates on approval)
/sprint plan 2 weeks
/sprint triage     # label/dedupe/de-stale the open issues
/sprint work       # dispatch next unblocked issue to a worker subagent + reviewer, opens PR
/sprint work #12 #14   # dispatch specific issues (parallel worktrees if independent)
/sprint review     # advisory review pass on open PRs
/sprint retro      # close out the sprint, write SPRINTS.md, roll over slippage
```

The full loop: `/sprint plan` → `/sprint work` (repeat until the milestone
is empty; you merge the PRs) → `/sprint retro` → repeat. Workers implement
against acceptance criteria in isolated worktrees; an independent reviewer
subagent (fresh eyes, diff only) gates every PR. Merging is always yours.

## Kanban board (one-time setup)

State lives in `status/*` labels — the board just visualizes them:

1. Run `scripts/sprint-labels.sh` in the repo (creates status/area/prio/size labels).
2. Create a Projects v2 board: `gh project create --owner <you> --title "<repo> board"`.
3. In the project UI: Board layout → **group by Labels** → show only the
   `status/*` labels as columns, ordered Backlog / Ready / Blocked /
   In progress / In review. Add a "Done" column from closed items if desired.
4. Optional automations (project Settings → Workflows): auto-add new issues
   from the repo; auto-archive closed items.

Columns then move themselves: `/sprint` flips labels at each transition
(ready → in-progress at dispatch, → in-review at PR open, closed on merge),
and dragging a card manually flips the label back — one source of truth.

Everything runs in your interactive Claude Code session — subscription-
compliant, no CI tokens, no server.
