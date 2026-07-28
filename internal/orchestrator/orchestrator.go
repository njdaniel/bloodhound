// Package orchestrator owns the bloodhound state machine:
//
//	intake → plan → investigate (parallel hounds) → verify → (replan?) → report
//
// It is deliberately hand-rolled rather than framework-based: budgets,
// checkpoints, and timeouts are explicit and inspectable.
// See specs/001-build-spec.md §3.1.
package orchestrator

import "time"

// Phase is a stage in the investigation state machine.
type Phase string

const (
	PhaseIntake      Phase = "intake"
	PhasePlan        Phase = "plan"
	PhaseInvestigate Phase = "investigate"
	PhaseVerify      Phase = "verify"
	PhaseReport      Phase = "report"
	PhaseDone        Phase = "done"
)

// Case is one incident investigation, from alert to report.
type Case struct {
	ID          string            `json:"id"`
	AlertName   string            `json:"alert_name"`
	Labels      map[string]string `json:"labels"`
	FiringSince time.Time         `json:"firing_since"`
	Phase       Phase             `json:"phase"`
	WorkDir     string            `json:"work_dir"`
}

// Budget caps what an agent may spend during a case.
type Budget struct {
	MaxTokens    int           `json:"max_tokens"`
	MaxToolCalls int           `json:"max_tool_calls"`
	MaxWallClock time.Duration `json:"max_wall_clock"`
}

// Finding is the structured output of one investigator hound: version 1 of
// the schema checked in at internal/agents/hounds/schema/finding.v1.json
// (spec 002 §3.3). The Go type and that schema change together; pre-1.0 there
// is no compatibility shim.
type Finding struct {
	// Hound names the investigator that produced this finding, e.g. "metrics".
	Hound string `json:"hound"`
	// Summary is the hypothesis and the shape of the supporting data.
	Summary string `json:"summary"`
	// Confidence is the hound's self-assessed confidence, 0..1.
	Confidence float64 `json:"confidence"`
	// Onset is when the anomaly began, if the hound determined it.
	Onset *time.Time `json:"onset,omitempty"`
	// Evidence is the queries run and what they showed, at most 12 records.
	// CaptureRef values are assigned by the hound loop, never by the model
	// (spec 002 §3.4).
	Evidence []Evidence `json:"evidence"`
	// DeadEnds is what the hound ruled out and why, at most 8 records. Dead
	// ends are first-class output: they are what makes a report trustworthy.
	DeadEnds []DeadEnd `json:"dead_ends"`
}

// Evidence is one query the hound ran and what it observed, anchored to the
// on-disk capture of that tool call.
type Evidence struct {
	// Tool is the MCP tool that produced the observation, e.g. "query_range".
	Tool string `json:"tool"`
	// Query is the tool input the claim rests on, e.g. the PromQL expression.
	Query string `json:"query"`
	// Observation is what the result showed, in the hound's words.
	Observation string `json:"observation"`
	// CaptureRef is the capture filename under the case work dir's captures/
	// directory, e.g. "mcp/003-query_range.json". Injected by the hound loop
	// from the tool-call sequence number the model cited (spec 002 §3.4), so
	// the M4 evidence-honesty grader can check claims mechanically.
	CaptureRef string `json:"capture_ref"`
}

// DeadEnd is a theory the hound considered and eliminated, with the reason.
type DeadEnd struct {
	// Theory is the hypothesis that was considered.
	Theory string `json:"theory"`
	// RuledOutBy is the observation that eliminated it.
	RuledOutBy string `json:"ruled_out_by"`
}

// Spend reports what one agent run consumed. It matches the spend object of
// the checkpoint format (spec 002 §4.2) so checkpoints can embed it verbatim
// and `bloodhound cost` can sum them.
type Spend struct {
	// InputTokens is the total prompt tokens across every model call.
	InputTokens int `json:"input_tokens"`
	// OutputTokens is the total completion tokens across every model call.
	OutputTokens int `json:"output_tokens"`
	// USD is the provider-priced dollar cost of those tokens.
	USD float64 `json:"usd"`
	// ToolCalls is how many MCP tool calls the run made.
	ToolCalls int `json:"tool_calls"`
	// WallMS is the run's wall-clock duration in milliseconds.
	WallMS int64 `json:"wall_ms"`
}
