// Package orchestrator owns the bloodhound state machine:
//
//	intake → plan → investigate (parallel hounds) → verify → (replan?) → report
//
// It is deliberately hand-rolled rather than framework-based: budgets,
// checkpoints, and timeouts are explicit and inspectable.
// See specs/001-build-spec.md §3.1.
//
// The M1 walk is the linear v0 pipeline — intake → investigate → report → done
// (spec 002 §4.1). The transition table is data (Pipeline), so M2 inserts plan
// and verify by shipping a new table rather than editing the walk.
//
// Every phase writes a checkpoint before the case file advances, and every
// file in the work dir is written atomically, so a case survives a crash at
// any point and resumes from its checkpoints without re-executing work that
// already completed (spec 002 §4.2, §4.3):
//
//	o, err := orchestrator.New(orchestrator.Options{
//		Root:        "work",
//		Investigate: investigate, // internal/agents/hounds.Run
//		Report:      reporter.Render,
//	})
//	res, err := o.Hunt(ctx, "alert.json")   // or o.Resume(ctx, caseID)
//
// The investigate and report phases are injected rather than imported: the
// packages that implement them depend on this one's types, and the dependency
// only points one way.
package orchestrator

import "time"

// Phase is a stage in the investigation state machine.
type Phase string

// The phases of the state machine. PhasePlan and PhaseVerify are reserved for
// M2: they are declared here so checkpoints and case files written today keep
// meaning tomorrow, but the v0 pipeline does not visit them (spec 002 §4.1).
// PhaseDone and PhaseFailed are terminal — PhaseFailed for the process, not
// for the case, which stays resumable once the cause is fixed.
const (
	PhaseIntake      Phase = "intake"
	PhasePlan        Phase = "plan"
	PhaseInvestigate Phase = "investigate"
	PhaseVerify      Phase = "verify"
	PhaseReport      Phase = "report"
	PhaseDone        Phase = "done"
	PhaseFailed      Phase = "failed"
)

// Case is one incident investigation, from alert to report. It is persisted as
// case.json in the work dir and is the resume cursor: Phase says what to run
// next (spec 002 §4.2).
type Case struct {
	// ID is the case identifier, c-<utc yyyymmddThhmmss>-<6 hex>.
	ID string `json:"id"`
	// AlertName is the alert's "alertname" label, set by the intake phase.
	AlertName string `json:"alert_name"`
	// Labels are the alert's labels, set by the intake phase.
	Labels map[string]string `json:"labels"`
	// FiringSince is when the alert started firing, set by the intake phase.
	FiringSince time.Time `json:"firing_since"`
	// Phase is the phase to run next, or PhaseDone/PhaseFailed.
	Phase Phase `json:"phase"`
	// WorkDir is the case work dir, work/<case-id>.
	WorkDir string `json:"work_dir"`
	// AlertPath is the alert file the case was opened from. Recorded so that
	// resume can re-run a failed intake against the corrected file without
	// the operator having to re-supply it (spec 002 §4.3).
	AlertPath string `json:"alert_path,omitempty"`
	// Pipeline is the version of the transition table the case is being
	// walked with, e.g. PipelineV0. M2 reads it to avoid reinterpreting an
	// in-flight case's checkpoints under a new walk.
	Pipeline string `json:"pipeline,omitempty"`
}

// Budget caps what an agent may spend during a case. A zero field means the
// package default (DefaultBudget); a negative field disables that cap, which
// only tests and deliberate overrides should want.
type Budget struct {
	MaxTokens    int           `json:"max_tokens"`
	MaxToolCalls int           `json:"max_tool_calls"`
	MaxWallClock time.Duration `json:"max_wall_clock"`
}

// Budget defaults for one hound run (spec 002 §3.2). They live here rather
// than in internal/agents/hounds because the orchestrator needs the wall-clock
// cap to size the investigate phase timeout, and hounds already depends on
// this package — the reverse edge would be an import cycle.
const (
	// DefaultMaxToolCalls caps MCP tool calls per hound run.
	DefaultMaxToolCalls = 12
	// DefaultMaxTokens caps total (input + output) tokens per hound run.
	DefaultMaxTokens = 50_000
	// DefaultMaxWallClock caps a hound run's wall-clock duration.
	DefaultMaxWallClock = 3 * time.Minute
)

// DefaultBudget returns the M1 per-hound budget.
func DefaultBudget() Budget {
	return Budget{
		MaxTokens:    DefaultMaxTokens,
		MaxToolCalls: DefaultMaxToolCalls,
		MaxWallClock: DefaultMaxWallClock,
	}
}

// WithDefaults fills unset budget fields with the package defaults.
func (b Budget) WithDefaults() Budget {
	d := DefaultBudget()
	if b.MaxToolCalls == 0 {
		b.MaxToolCalls = d.MaxToolCalls
	}
	if b.MaxTokens == 0 {
		b.MaxTokens = d.MaxTokens
	}
	if b.MaxWallClock == 0 {
		b.MaxWallClock = d.MaxWallClock
	}
	return b
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

// Add returns the field-wise sum of two spends. Summing checkpoint spend is
// how `bloodhound cost` derives a case total and how resume charges already
// recorded spend against the remaining budget (spec 002 §4.2, §4.3).
func (s Spend) Add(o Spend) Spend {
	return Spend{
		InputTokens:  s.InputTokens + o.InputTokens,
		OutputTokens: s.OutputTokens + o.OutputTokens,
		USD:          s.USD + o.USD,
		ToolCalls:    s.ToolCalls + o.ToolCalls,
		WallMS:       s.WallMS + o.WallMS,
	}
}

// Normalize returns the spend with every quantity clamped to zero or above.
// Spend records resources consumed, so no field of it is meaningful negative.
//
// It exists for checkpoints written before the issue #46 fix, when `.UTC()`
// stripping the monotonic clock reading let a backwards clock step mid-phase
// record a negative WallMS. Clamping at the write site cannot repair a file
// that is already on disk, and a negative WallMS read back is not merely a
// wrong ledger line: remainingBudget and phaseTimeout each subtract it from a
// cap, so it hands the next attempt more than its full budget *and* a longer
// deadline than a fresh case gets. LoadCheckpoints normalizes what it reads so
// that both consumers, and `bloodhound cost`, see a coherent figure.
func (s Spend) Normalize() Spend {
	if s.InputTokens < 0 {
		s.InputTokens = 0
	}
	if s.OutputTokens < 0 {
		s.OutputTokens = 0
	}
	if s.USD < 0 {
		s.USD = 0
	}
	if s.ToolCalls < 0 {
		s.ToolCalls = 0
	}
	if s.WallMS < 0 {
		s.WallMS = 0
	}
	return s
}

// Tokens returns the total input plus output tokens.
func (s Spend) Tokens() int { return s.InputTokens + s.OutputTokens }
