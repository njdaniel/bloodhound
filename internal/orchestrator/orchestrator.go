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

// Finding is the structured output of one investigator hound.
type Finding struct {
	Hound      string   `json:"hound"`
	Summary    string   `json:"summary"`
	Confidence float64  `json:"confidence"` // 0..1
	Evidence   []string `json:"evidence"`   // captured tool-result references
	DeadEnds   []string `json:"dead_ends"`  // what was ruled out, and why
}
