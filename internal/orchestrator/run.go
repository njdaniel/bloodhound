package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/njdaniel/bloodhound/internal/intake"
)

// Per-phase timeouts (spec 002 §4.1). The investigate phase gets the hound's
// own wall-clock budget plus a grace window, so the hound hits its budget and
// submits a best-effort finding before the orchestrator's deadline fires: the
// phase timeout is the backstop for a wedged hound, not the normal control.
const (
	// IntakeTimeout bounds parsing the alert and opening the case.
	IntakeTimeout = 10 * time.Second
	// ReportTimeout bounds rendering and writing the report artifacts.
	ReportTimeout = 30 * time.Second
	// InvestigateGrace is added to the hound wall-clock budget.
	InvestigateGrace = 30 * time.Second
)

// ErrBadAlert marks a case that cannot start because its alert file is
// missing, unreadable, or not a usable Alertmanager alert. The CLI maps it to
// exit code 2; every other phase failure is exit 1.
var ErrBadAlert = errors.New("orchestrator: unusable alert file")

// ErrBudgetExhausted reports that spend already recorded in this case's
// checkpoints leaves no budget for the phase about to run. It is what stops a
// crash loop from multiplying cost across resumes (spec 002 §4.3).
var ErrBudgetExhausted = errors.New("orchestrator: budget exhausted by prior attempts")

// InvestigateFunc runs the investigation for a case and reports what it cost.
// The orchestrator injects it rather than importing the hounds package, which
// depends on this one; the CLI wires internal/agents/hounds.Run.
//
// ctx carries the investigate phase deadline. Anything with a lifetime longer
// than one phase — an MCP server session in particular — must be built on a
// context the caller owns, not on this one.
type InvestigateFunc func(ctx context.Context, c Case, budget Budget) (Finding, Spend, error)

// ReportInput is everything the reporter renders from.
type ReportInput struct {
	// Case is the investigated case with its intake facts filled in.
	Case Case
	// Finding is the investigate phase's result.
	Finding Finding
	// Spend is the case total so far — the report's spend footer.
	Spend Spend
	// GeneratedAt is the report timestamp, from the orchestrator's clock so
	// that tests can make report artifacts byte-stable.
	GeneratedAt time.Time
}

// Artifact is one file the reporter produced, to be written into the case work
// dir. The orchestrator does the writing so that every file in the work dir
// goes through the same atomic path.
type Artifact struct {
	// Name is the work-dir-relative path, e.g. "report.json".
	Name string
	// Data is the file's full content.
	Data []byte
}

// ReportFunc renders a finished case into work dir artifacts. The CLI wires
// internal/agents/reporter.Render.
type ReportFunc func(ctx context.Context, in ReportInput) ([]Artifact, error)

// Options configures an Orchestrator.
type Options struct {
	// Root is the work root that case directories are created under, e.g.
	// "work". Required.
	Root string
	// Pipeline is the transition table to walk. The zero value means V0.
	Pipeline Pipeline
	// Budget caps the investigate phase. Zero fields take the package
	// defaults; the wall-clock cap also sizes the phase timeout.
	Budget Budget
	// Investigate runs the investigate phase. Required.
	Investigate InvestigateFunc
	// Report renders the report phase's artifacts. Required.
	Report ReportFunc
	// Now reads the clock; nil means time.Now.
	Now func() time.Time
	// NewID mints case IDs; nil means NewCaseID from the clock. Tests
	// override it to get stable work dir names.
	NewID func() (string, error)

	// rename swaps a temp file into place during atomic writes; nil means
	// os.Rename. Only the torn-write test replaces it.
	rename func(oldpath, newpath string) error
	// timeout overrides the per-phase deadline; nil means the spec 002 §4.1
	// defaults. Only the phase-timeout test replaces it — the real timeouts
	// are measured in seconds and minutes, which no test should wait out.
	timeout func(phase Phase) time.Duration
}

// Result is a completed walk: the case as it ended, the finding it produced,
// the artifacts written, and what the whole case cost.
type Result struct {
	// Case is the final case, with Phase = PhaseDone.
	Case Case
	// Finding is the investigate phase's finding.
	Finding Finding
	// Artifacts are the work-dir-relative report artifact paths.
	Artifacts []string
	// Spend is the sum of every checkpoint's spend.
	Spend Spend
}

// Orchestrator walks a case through the pipeline, checkpointing each phase.
// It is safe to reuse across cases but a single case must be walked by one
// Orchestrator at a time — the work dir has no lock.
type Orchestrator struct {
	opts   Options
	phases map[Phase]phaseFunc
	walk   []Phase
}

// phaseFunc is one phase of the machine (spec 002 §4.1): it runs under the
// phase's own deadline and returns the value recorded as the checkpoint's
// output.
type phaseFunc func(ctx context.Context, r *run) (any, error)

// New validates opts and returns an Orchestrator.
func New(opts Options) (*Orchestrator, error) {
	if opts.Root == "" {
		return nil, errors.New("orchestrator: Options.Root is required")
	}
	if opts.Investigate == nil {
		return nil, errors.New("orchestrator: Options.Investigate is required")
	}
	if opts.Report == nil {
		return nil, errors.New("orchestrator: Options.Report is required")
	}
	if opts.Pipeline.Start == "" {
		opts.Pipeline = V0()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.rename == nil {
		opts.rename = os.Rename
	}
	opts.Budget = opts.Budget.WithDefaults()

	walk, err := opts.Pipeline.Walk()
	if err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}
	o := &Orchestrator{opts: opts, walk: walk}
	o.phases = map[Phase]phaseFunc{
		PhaseIntake:      o.phaseIntake,
		PhaseInvestigate: o.phaseInvestigate,
		PhaseReport:      o.phaseReport,
	}
	for _, p := range walk {
		if _, ok := o.phases[p]; !ok {
			return nil, fmt.Errorf("orchestrator: pipeline %q visits phase %q, which has no implementation", opts.Pipeline.Version, p)
		}
	}
	return o, nil
}

// now reads the configured clock in UTC.
func (o *Orchestrator) now() time.Time { return o.opts.Now().UTC() }

// Hunt opens a new case for the alert file at alertPath and walks it to
// completion. The case work dir is created before the first phase runs, so a
// failure in intake still leaves a failed checkpoint to resume from once the
// alert file is fixed.
func (o *Orchestrator) Hunt(ctx context.Context, alertPath string) (Result, error) {
	if alertPath == "" {
		return Result{}, fmt.Errorf("%w: no alert file given", ErrBadAlert)
	}
	id, err := o.newID()
	if err != nil {
		return Result{}, err
	}
	dir := filepath.Join(o.opts.Root, id)
	st, err := newStore(dir, o.opts.rename)
	if err != nil {
		return Result{}, fmt.Errorf("opening case %s: %w", id, err)
	}
	c := Case{
		ID:        id,
		Phase:     o.opts.Pipeline.Start,
		WorkDir:   dir,
		AlertPath: alertPath,
		Pipeline:  o.opts.Pipeline.Version,
	}
	if err := st.writeCase(c); err != nil {
		return Result{}, fmt.Errorf("opening case %s: %w", id, err)
	}
	return o.run(ctx, &run{o: o, store: st, c: c})
}

// Resume continues the case with the given ID from its checkpoints: phases
// with a completed checkpoint are loaded rather than re-executed, and the
// first phase without one runs next (spec 002 §4.3). A failed checkpoint is
// overwritten when its phase runs again.
func (o *Orchestrator) Resume(ctx context.Context, caseID string) (Result, error) {
	if caseID == "" {
		return Result{}, errors.New("orchestrator: no case ID given")
	}
	dir := filepath.Join(o.opts.Root, caseID)
	st, err := openStore(dir, o.opts.rename)
	if err != nil {
		return Result{}, fmt.Errorf("resuming case %s: %w", caseID, err)
	}
	c, err := st.readCase()
	if err != nil {
		return Result{}, fmt.Errorf("resuming case %s: %w", caseID, err)
	}
	// The work dir may have been moved or copied since it was written; the
	// path we were asked to resume is the truth.
	c.WorkDir = dir
	if c.ID == "" {
		c.ID = caseID
	}
	r := &run{o: o, store: st, c: c}
	if err := r.loadCheckpoints(); err != nil {
		return Result{}, fmt.Errorf("resuming case %s: %w", caseID, err)
	}
	return o.run(ctx, r)
}

// run is one walk of the pipeline over one case.
type run struct {
	o     *Orchestrator
	store *store
	c     Case

	// completed holds the loaded checkpoints of phases that already ran, by
	// phase. Their outputs are deserialized instead of re-executed.
	completed map[Phase]Checkpoint
	// carried is the spend of failed attempts at a phase, by phase. A re-run
	// overwrites the failed checkpoint (spec 002 §4.3), so its spend is
	// folded into the replacement — otherwise the money the failed attempt
	// cost would vanish from the checkpoint sum that `bloodhound cost` and
	// the next resume's budget both read.
	carried map[Phase]Spend
	// prior is the spend of every checkpoint on disk plus every phase this
	// walk has run: what the case has cost so far.
	prior Spend
	// phaseSpend is set by a phase to report what it consumed, and consumed
	// by runPhase when it writes the checkpoint.
	phaseSpend Spend

	// finding and artifacts carry phase outputs between phases.
	finding   Finding
	artifacts []string
}

// loadCheckpoints reads the case's checkpoints, adopting the outputs of
// completed phases and charging all recorded spend — failed attempts
// included — against the remaining budget.
func (r *run) loadCheckpoints() error {
	cps, err := LoadCheckpoints(r.c.WorkDir)
	if err != nil {
		return err
	}
	r.completed = make(map[Phase]Checkpoint, len(cps))
	r.carried = make(map[Phase]Spend, len(cps))
	for _, cp := range cps {
		r.prior = r.prior.Add(cp.Spend)
		if cp.Status == StatusCompleted {
			r.completed[cp.Phase] = cp
			continue
		}
		r.carried[cp.Phase] = r.carried[cp.Phase].Add(cp.Spend)
	}
	return nil
}

// run walks the pipeline, executing or loading each phase in order.
func (o *Orchestrator) run(ctx context.Context, r *run) (Result, error) {
	if r.completed == nil {
		r.completed = map[Phase]Checkpoint{}
	}
	if r.carried == nil {
		r.carried = map[Phase]Spend{}
	}
	for i, phase := range o.walk {
		index := i + 1
		if cp, ok := r.completed[phase]; ok {
			if err := r.adopt(phase, cp); err != nil {
				return Result{}, fmt.Errorf("case %s: loading %s checkpoint: %w", r.c.ID, phase, err)
			}
			continue
		}
		if err := o.runPhase(ctx, r, phase, index); err != nil {
			return Result{}, err
		}
	}
	r.c.Phase = PhaseDone
	if err := r.store.writeCase(r.c); err != nil {
		return Result{}, fmt.Errorf("case %s: %w", r.c.ID, err)
	}
	return Result{
		Case:      r.c,
		Finding:   r.finding,
		Artifacts: r.artifacts,
		Spend:     r.prior,
	}, nil
}

// adopt deserializes a completed checkpoint's output as that phase's result,
// so a resumed walk continues with the same state the original run had.
func (r *run) adopt(phase Phase, cp Checkpoint) error {
	switch phase {
	case PhaseIntake:
		var c Case
		if err := json.Unmarshal(cp.Output, &c); err != nil {
			return err
		}
		r.c.AlertName = c.AlertName
		r.c.Labels = c.Labels
		r.c.FiringSince = c.FiringSince
		if r.c.AlertPath == "" {
			r.c.AlertPath = c.AlertPath
		}
	case PhaseInvestigate:
		var f Finding
		if err := json.Unmarshal(cp.Output, &f); err != nil {
			return err
		}
		r.finding = f
	case PhaseReport:
		var out ReportOutput
		if err := json.Unmarshal(cp.Output, &out); err != nil {
			return err
		}
		r.artifacts = out.Artifacts
	default:
		return fmt.Errorf("no loader for phase %q", phase)
	}
	return nil
}

// runPhase executes one phase under its own deadline and records the outcome.
// Ordering is load-bearing: the checkpoint lands before case.json advances, so
// a crash in between leaves a case whose next phase is already checkpointed —
// which resume skips rather than repeats (spec 002 §4.2).
//
// There are no phase-level retries in v0. Transient faults are the llm
// middleware's job; a phase that still fails is a bug or an outage, and either
// way stopping is honest.
func (o *Orchestrator) runPhase(ctx context.Context, r *run, phase Phase, index int) error {
	fn, ok := o.phases[phase]
	if !ok {
		return fmt.Errorf("case %s: phase %q has no implementation", r.c.ID, phase)
	}
	timeout := o.phaseTimeout(phase)
	started := o.now()

	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	r.phaseSpend = Spend{}
	out, phaseErr := fn(pctx, r)
	finished := o.now()

	spend := r.phaseSpend
	spend.WallMS = finished.Sub(started).Milliseconds()
	r.prior = r.prior.Add(spend)

	cp := Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		CaseID:        r.c.ID,
		Phase:         phase,
		StartedAt:     started,
		FinishedAt:    finished,
		// The checkpoint records what this phase has cost the case, not just
		// this attempt: it replaces any failed checkpoint for the phase, and
		// the sum of checkpoints must stay the case's true spend.
		Spend: spend.Add(r.carried[phase]),
	}

	if phaseErr != nil {
		phaseErr = annotateDeadline(pctx, phase, timeout, phaseErr)
		msg := phaseErr.Error()
		cp.Status = StatusFailed
		cp.Error = &msg
		cp.Output = json.RawMessage("null")
		if err := r.store.writeCheckpoint(index, cp); err != nil {
			return errors.Join(phaseErr, fmt.Errorf("case %s: %w", r.c.ID, err))
		}
		r.c.Phase = PhaseFailed
		if err := r.store.writeCase(r.c); err != nil {
			return errors.Join(phaseErr, fmt.Errorf("case %s: %w", r.c.ID, err))
		}
		return fmt.Errorf("case %s: phase %s: %w", r.c.ID, phase, phaseErr)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("case %s: marshaling %s output: %w", r.c.ID, phase, err)
	}
	cp.Status = StatusCompleted
	cp.Output = raw
	if err := r.store.writeCheckpoint(index, cp); err != nil {
		return fmt.Errorf("case %s: %w", r.c.ID, err)
	}

	r.c.Phase = o.opts.Pipeline.Next[phase]
	if err := r.store.writeCase(r.c); err != nil {
		return fmt.Errorf("case %s: %w", r.c.ID, err)
	}
	return nil
}

// phaseTimeout returns the deadline for one phase (spec 002 §4.1).
func (o *Orchestrator) phaseTimeout(phase Phase) time.Duration {
	if o.opts.timeout != nil {
		return o.opts.timeout(phase)
	}
	switch phase {
	case PhaseInvestigate:
		if o.opts.Budget.MaxWallClock > 0 {
			return o.opts.Budget.MaxWallClock + InvestigateGrace
		}
		return DefaultMaxWallClock + InvestigateGrace
	case PhaseReport:
		return ReportTimeout
	default:
		return IntakeTimeout
	}
}

// annotateDeadline makes a phase timeout legible in the checkpoint even when
// the phase reported the deadline in its own words (or swallowed it): the
// recorded error names the phase and the timeout, and errors.Is still matches
// context.DeadlineExceeded.
func annotateDeadline(pctx context.Context, phase Phase, timeout time.Duration, err error) error {
	if !errors.Is(pctx.Err(), context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s timed out after %s: %w: %v", phase, timeout, context.DeadlineExceeded, err)
}

// phaseIntake parses the alert file into the case and returns the Case as the
// checkpoint output (spec 002 §4.1). The work dir already exists: Hunt creates
// it so that a bad alert file still produces a resumable case.
func (o *Orchestrator) phaseIntake(ctx context.Context, r *run) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.c.AlertPath == "" {
		return nil, fmt.Errorf("%w: case %s records no alert file", ErrBadAlert, r.c.ID)
	}
	alert, err := intake.ParseFile(r.c.AlertPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadAlert, err)
	}
	r.c.AlertName = alert.Name
	r.c.Labels = alert.Labels
	r.c.FiringSince = alert.StartsAt
	return r.c, nil
}

// phaseInvestigate runs the hound against the remaining budget and returns the
// Finding as the checkpoint output.
func (o *Orchestrator) phaseInvestigate(ctx context.Context, r *run) (any, error) {
	budget, err := remainingBudget(o.opts.Budget, r.prior)
	if err != nil {
		return nil, err
	}
	finding, spend, err := o.opts.Investigate(ctx, r.c, budget)
	// Spend is recorded even on failure: a hound that burned tokens and then
	// died still cost money, and the next resume must be charged for it.
	r.phaseSpend = spend
	if err != nil {
		return nil, err
	}
	r.finding = finding
	return finding, nil
}

// phaseReport renders the report artifacts, writes them atomically into the
// work dir, and returns their paths as the checkpoint output.
func (o *Orchestrator) phaseReport(ctx context.Context, r *run) (any, error) {
	artifacts, err := o.opts.Report(ctx, ReportInput{
		Case:        r.c,
		Finding:     r.finding,
		Spend:       r.prior,
		GeneratedAt: o.now(),
	})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		if err := r.store.writeFile(a.Name, a.Data); err != nil {
			return nil, err
		}
		names = append(names, a.Name)
	}
	r.artifacts = names
	return ReportOutput{Artifacts: names}, nil
}

// remainingBudget subtracts spend already recorded for this case from the
// configured budget. A cap that is already used up fails the phase instead of
// handing the hound a zero budget, which would silently mean "default"
// (spec 002 §4.3). Negative caps are disabled and pass through untouched.
func remainingBudget(b Budget, spent Spend) (Budget, error) {
	out := b
	if b.MaxTokens > 0 {
		out.MaxTokens = b.MaxTokens - spent.Tokens()
		if out.MaxTokens <= 0 {
			return Budget{}, fmt.Errorf("%w: %d of %d tokens already spent", ErrBudgetExhausted, spent.Tokens(), b.MaxTokens)
		}
	}
	if b.MaxToolCalls > 0 {
		out.MaxToolCalls = b.MaxToolCalls - spent.ToolCalls
		if out.MaxToolCalls <= 0 {
			return Budget{}, fmt.Errorf("%w: %d of %d tool calls already made", ErrBudgetExhausted, spent.ToolCalls, b.MaxToolCalls)
		}
	}
	if b.MaxWallClock > 0 {
		out.MaxWallClock = b.MaxWallClock - time.Duration(spent.WallMS)*time.Millisecond
		if out.MaxWallClock <= 0 {
			return Budget{}, fmt.Errorf("%w: %s of %s wall clock already used",
				ErrBudgetExhausted, time.Duration(spent.WallMS)*time.Millisecond, b.MaxWallClock)
		}
	}
	return out, nil
}

// newID mints the case ID for a new case.
func (o *Orchestrator) newID() (string, error) {
	if o.opts.NewID != nil {
		id, err := o.opts.NewID()
		if err != nil {
			return "", fmt.Errorf("minting case ID: %w", err)
		}
		if id == "" {
			return "", errors.New("orchestrator: Options.NewID returned an empty ID")
		}
		return id, nil
	}
	return NewCaseID(o.now())
}

// NewCaseID mints a case ID of the form c-<utc yyyymmddThhmmss>-<6 hex>
// (spec 002 §4.1). The timestamp makes work dirs sort chronologically; the
// crypto/rand suffix keeps two alerts in the same second from colliding.
func NewCaseID(now time.Time) (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("reading random case ID suffix: %w", err)
	}
	return fmt.Sprintf("c-%s-%s", now.UTC().Format("20060102T150405"), hex.EncodeToString(b[:])), nil
}

// Cost returns the checkpoints of the case with the given ID under root and
// their spend total. It is the whole of `bloodhound cost`: the checkpoints are
// the ledger (spec 002 §4.2).
func Cost(root, caseID string) (Case, []Checkpoint, Spend, error) {
	dir := filepath.Join(root, caseID)
	c, err := ReadCase(dir)
	if err != nil {
		return Case{}, nil, Spend{}, fmt.Errorf("reading case %s: %w", caseID, err)
	}
	c.WorkDir = dir
	cps, err := LoadCheckpoints(dir)
	if err != nil {
		return Case{}, nil, Spend{}, fmt.Errorf("reading case %s: %w", caseID, err)
	}
	return c, cps, SumSpend(cps), nil
}
