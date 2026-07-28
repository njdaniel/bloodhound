// Package hounds implements the investigator agents: bounded LLM tool-use
// loops whose tools are exactly one MCP server's tools, terminating in a
// schema-valid orchestrator.Finding. metrics-hound (mcp-prom) is the M1
// implementation; the loop plumbing here is what the M2 hounds reuse.
//
// Three mechanisms carry the design (spec 002 §3.2–3.4):
//
//   - Termination is forced tool use. The model ends its run by calling the
//     synthetic submit_finding tool, whose input schema is derived from the
//     checked-in Finding schema. No prose is ever parsed for a conclusion.
//   - Budgets are checked before every model call — tool calls, tokens, and
//     wall clock. Exhausting one does not truncate the run: it forces a
//     best-effort submission, so a run that ran out of room still produces a
//     finding with its unverified theories recorded as dead ends.
//   - Evidence refs are loop-assigned. The model cites a tool call by the
//     sequence number the loop stamped on that call's result; the loop
//     resolves it to the capture file mcpclient wrote. The model never
//     authors a capture_ref, which is what makes the M4 evidence-honesty
//     grader mechanical.
//
// The Finding schema (schema/finding.v1.json) and the system prompt
// (prompt/metrics.v1.md) are checked-in, embedded, versioned assets rather
// than runtime-built strings: both are contracts, and contracts should be
// reviewable as diffs.
package hounds
