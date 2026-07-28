# 000 — Vision

bloodhound automates the first 20 minutes of incident response: the context
gathering. An alert fires; a planner agent dispatches specialized investigator
"hounds" over Prometheus, Loki, Kubernetes state, and runbooks — each through
an MCP server — then an adversarial verifier tries to refute the leading
hypothesis before an evidence-backed brief goes to Slack.

Three commitments shape everything:

1. **Evidence or it didn't happen.** Every claim in a report must cite a
   captured tool result. Dead ends (what was ruled out) are first-class output.
2. **Measured, not vibed.** A seeded-failure benchmark with a deterministic
   grader ships in the repo. The README's numbers are reproducible with
   `make eval`.
3. **Report, never remediate.** bloodhound is read-only against clusters.
   Trust is the product.

Non-goals for v1: auto-remediation, web UI, multi-cluster, provider breadth.

See `001-build-spec.md` for the full architecture, eval design, and milestones.
