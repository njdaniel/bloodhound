---
description: Goal-oriented sprint workflow — plan the next sprint, create and triage issues, run retros
---

# /sprint — goal-oriented sprint workflow

Mode is taken from arguments: `plan` (default), `triage`, `work`, `review`,
or `retro`.
Arguments: $ARGUMENTS

You are the sprint planner for this repo. The roadmap milestones (M0–M5) in
README.md and `specs/001-build-spec.md` are the goals; sprints are the
increments that reach them. Issues are the single source of truth for work —
no side lists.

## Ground rules (all modes)

- GATHER CONTEXT FIRST, in this order: `CLAUDE.md`, README roadmap,
  `specs/` (latest numbered specs), then:
  - `gh issue list --state open --limit 100 --json number,title,labels,milestone,updatedAt`
  - `gh api repos/{owner}/{repo}/milestones --jq '.[] | {title, open_issues, closed_issues}'`
  - `git log --oneline -20` and the most recent entries in `SPRINTS.md` if it exists.
- PROPOSE BEFORE ACTING. Present the full set of intended changes (issues to
  create, labels to apply, closures, comments) and STOP for explicit approval.
  Never create, edit, close, or comment on anything before the user approves.
- All writes go through `gh` commands. Echo each command as you run it.
- Never delete issues. Closing requires a comment explaining why.
- Max 9 issues per sprint. If the plan needs more, the sprint goal is too big —
  split the goal instead.
- Every issue must trace to a spec section (link it). If work has no spec
  backing, the first issue is "write the spec for X," not the implementation.
- If `scripts/sprint-labels.sh` exists and required labels are missing, run it
  once (after approval) to create the label set.

## Status labels (Kanban state — labels are the single source of truth)

The Projects board groups by these labels; keeping them correct IS keeping
the board correct. Invariants:

- Every OPEN issue has exactly one `status/*` label. Fix violations on sight
  (during any mode) — they're board corruption, not style nits.
- "Done" is never a label. Done = the issue is closed (via the PR's
  `closes #n`).
- Transitions and their owners:
  - created in sprint, unblocked            → `status/ready`      (plan)
  - created in sprint, has open blockers    → `status/blocked`    (plan)
  - created/moved outside the sprint        → `status/backlog`    (plan/triage)
  - all blockers now closed                 → `status/blocked` → `status/ready` (work, start of every run)
  - worker dispatched                       → `status/ready` → `status/in-progress` (work)
  - PR opened                               → `status/in-progress` → `status/in-review` (work)
  - reviewer requested changes (fix cycle)  → stays `status/in-review`
  - dispatch aborted / worker failed        → back to `status/ready` with a comment (work)
  - rolled over at sprint end               → `status/ready` (next sprint) or `status/backlog` (retro)

At the START of every `work` run, do a sync pass first: check `status/blocked`
issues whose blockers are all closed and promote them to `status/ready`;
flag any open issue missing a status label.

## Mode: plan

1. Determine where we are: which roadmap milestone is active, what merged
   since the last sprint, what's still open from the previous sprint.
2. Propose ONE sprint goal — a single sentence describing a demoable outcome
   ("`bloodhound hunt` produces a real diagnosis from live Prometheus data"),
   tied to the active roadmap milestone. State the target sprint length the
   user gave, or ask if unknown.
3. Break the goal into 5–9 issues. For each, present:
   - Title (imperative: "Implement mcp-prom query_range tool")
   - Acceptance criteria (checkboxes — concrete, testable, includes "make
     check passes" where code is involved)
   - Spec link (file + section)
   - Size (S / M / L) and priority (p1 / p2 / p3)
   - Dependencies ("Blocked by: <issue title>")
   Also list what is explicitly OUT of this sprint, and the biggest risk.
4. STOP. Ask for approval or edits.
5. On approval:
   - Create a GitHub milestone `Sprint <N>: <short goal>` (N = next number).
   - Create each issue with `gh issue create`, applying `area/*`, `size/*`,
     `prio/*`, and the correct `status/*` label (`ready`, or `blocked` if it
     has dependencies) plus the sprint milestone. Put acceptance criteria,
     spec link, and "Blocked by #<n>" references in the body.
   - Finish with a summary table (number, title, size, deps) and a suggested
     order of attack, noting which issues are independent enough to run as
     parallel worktree sessions.

## Mode: triage

1. Fetch all open issues. Build a table: number, title, labels, milestone,
   days since update.
2. Propose a triage action per issue needing one:
   - Missing `area/*`, `size/*`, `prio/*`, or `status/*` labels → propose
     labels (status violations first — they corrupt the board).
   - Duplicate → propose closing with a comment linking the canonical issue.
   - Stale (>30 days, no longer matches specs) → propose close-with-comment
     or move to the `backlog` milestone.
   - Vague (no acceptance criteria) → draft criteria to add as a comment.
   - Orphaned (no milestone) → propose current sprint, backlog, or close.
3. Present the full action list. STOP for approval. Execute via `gh` only
   after approval, echoing each command.

## Mode: work

Dispatch the next work from the current sprint to implementation, worker by
worker. `$ARGUMENTS` may name specific issues (`/sprint work #12 #14`);
otherwise pick automatically.

1. From the current sprint milestone, select the next unblocked issue(s) in
   priority order. Default to ONE issue. Select up to three ONLY if they are
   size S/M, mutually independent (no shared packages per their spec links),
   and the user asked for parallel work.
2. Present the dispatch plan — issue(s), branch names, which run in parallel —
   and STOP for approval.
3. For each approved issue, run a WORKER as a subagent (parallel workers each
   get their own git worktree: `git worktree add ../<repo>-issue-<n> -b
   issue-<n>-<slug>`). Worker brief:
   - Read the issue body, its acceptance criteria, the linked spec section,
     and CLAUDE.md. Implement ONLY what the acceptance criteria require.
   - Run `make check`; iterate until green. Commit in small, described steps.
   - Return: what changed, criteria status (each checked or explicitly not),
     deviations from spec, anything discovered that belongs in a new issue.
4. For each completed worker, run a REVIEWER as a separate subagent with
   fresh eyes (it must not see the worker's reasoning, only the diff):
   - Input: the branch diff against the repo's DEFAULT branch —
     `git diff origin/HEAD...HEAD`. Never hardcode `main`: this repo's
     default branch is `master`, and diffing a branch that does not exist
     either errors or silently hands the reviewer the wrong changeset.
     Plus the issue, the linked spec, CLAUDE.md.
   - Check: acceptance criteria actually met; spec conformance; correctness
     and edge cases; tests real (assert behavior, not just run); no scope
     creep; conventions followed.
   - Verdict: APPROVE, or CHANGES with a concrete list.
5. On CHANGES: send the list back to the worker (max 2 fix cycles; then
   surface to the user). On APPROVE: push the branch and open the PR with
   `gh pr create` — body includes closes #<n>, criteria checklist, the
   reviewer's verdict summary, and which parts were agent-generated.
6. Report: table of issue → branch → PR → review outcome. The human merges;
   never merge, and never push directly to the default branch.

## Mode: review

Review open PRs (or `$ARGUMENTS`-named ones) that lack a review verdict.
Same reviewer brief as work-mode step 4, run per PR as a fresh subagent.
Post the result as a PR comment via `gh pr comment` (verdict, findings with
file:line, criteria checklist) after showing it to the user. Advisory only —
never approve/merge via `gh`, and never request changes on another human's
PR without the user seeing the comment first.

## Mode: retro

1. Compare the current sprint milestone's issues against merged PRs and
   closed issues. Compute: planned vs shipped, and what slipped.
2. Draft a `SPRINTS.md` entry (create the file if absent, newest entry on top):
   sprint number and goal, shipped (with PR links), slipped (with one-line
   reasons), metrics if available (PRs merged, eval score movement, notable
   costs), and one process observation worth keeping.
3. Propose dispositions for unfinished issues: roll to next sprint, move to
   backlog, or close-with-comment. STOP for approval.
4. On approval: write `SPRINTS.md`, apply issue moves, close the milestone.
   Suggest running `/sprint plan` next.
