package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/bloodhound/internal/seqname"
)

// alertFixture is a minimal Alertmanager webhook payload with one firing
// alert, written into the test's temp dir.
const alertFixture = `{
  "version": "4",
  "status": "firing",
  "alerts": [
    {
      "status": "firing",
      "labels": {"alertname": "KubePodCrashLooping", "namespace": "shop", "pod": "checkout-7d9"},
      "annotations": {"summary": "Pod is restarting"},
      "startsAt": "2026-07-28T10:02:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "fingerprint": "a1b2c3d4"
    }
  ]
}`

// writeAlert writes the fixture alert into dir and returns its path.
func writeAlert(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "alert.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing alert fixture: %v", err)
	}
	return path
}

// cannedFinding is what the fake hound submits.
func cannedFinding() Finding {
	return Finding{
		Hound:      "metrics",
		Summary:    "checkout restarts track a memory ceiling reached at 10:02.",
		Confidence: 0.8,
		Evidence: []Evidence{{
			Tool:        "query_range",
			Query:       `container_memory_working_set_bytes{pod="checkout-7d9"}`,
			Observation: "working set climbs to the limit and the pod is OOMKilled",
			CaptureRef:  "mcp/000-query_range.json",
		}},
		DeadEnds: []DeadEnd{{
			Theory:     "an upstream dependency started failing",
			RuledOutBy: "error rate on the upstream is flat across the window",
		}},
	}
}

// cannedSpend is what the fake hound reports consuming. WallMS is left zero:
// the orchestrator overwrites it with the measured phase duration.
func cannedSpend() Spend {
	return Spend{InputTokens: 1200, OutputTokens: 300, USD: 0.012, ToolCalls: 2}
}

// fakeHound is a canned InvestigateFunc with controllable delay and failure.
// It records every call so tests can assert a resumed phase was loaded rather
// than re-executed (spec 002 §5).
type fakeHound struct {
	mu      sync.Mutex
	calls   int
	budgets []Budget

	finding Finding
	spend   Spend
	// delay blocks the hound until it elapses or the phase context ends.
	delay time.Duration
	// err, when set, fails the run after the delay.
	err error
	// capture, when set, writes one capture file per call into this dir,
	// standing in for the llm/mcp capture streams.
	capture string
	// t, when set, fails the test if the hound is called at all.
	forbidden *testing.T
}

// run implements InvestigateFunc.
func (h *fakeHound) run(ctx context.Context, c Case, budget Budget) (Finding, Spend, error) {
	h.mu.Lock()
	h.calls++
	h.budgets = append(h.budgets, budget)
	n := h.calls
	h.mu.Unlock()

	if h.forbidden != nil {
		h.forbidden.Errorf("investigate ran again on resume; it must be loaded from its checkpoint")
	}
	if h.capture != "" {
		dir := filepath.Join(h.capture, "llm")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Finding{}, Spend{}, err
		}
		// seqname.Render, not a hand-rolled format: this stands in for the
		// real capture writers, so it has to name files the way they do.
		name := seqname.Render(n-1, "metrics-hound")
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"seq":0}`), 0o644); err != nil {
			return Finding{}, Spend{}, err
		}
	}
	if h.delay > 0 {
		select {
		case <-time.After(h.delay):
		case <-ctx.Done():
			return Finding{}, h.spend, ctx.Err()
		}
	}
	if h.err != nil {
		return Finding{}, h.spend, h.err
	}
	return h.finding, h.spend, nil
}

// count reports how many times the hound ran.
func (h *fakeHound) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// lastBudget returns the budget handed to the most recent call.
func (h *fakeHound) lastBudget(t *testing.T) Budget {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.budgets) == 0 {
		t.Fatal("hound was never called")
	}
	return h.budgets[len(h.budgets)-1]
}

// fixedReport is a ReportFunc producing two small artifacts, standing in for
// the real reporter so orchestrator tests do not depend on its formatting.
func fixedReport(_ context.Context, in ReportInput) ([]Artifact, error) {
	data, err := json.Marshal(map[string]any{
		"case_id":    in.Case.ID,
		"hypothesis": in.Finding.Summary,
		"spend":      in.Spend,
	})
	if err != nil {
		return nil, err
	}
	return []Artifact{
		{Name: ReportJSON, Data: data},
		{Name: ReportText, Data: []byte("report for " + in.Case.ID + "\n")},
	}, nil
}

// newTest builds an Orchestrator over root with the given hound, plus a stable
// case ID and clock so work dirs and timestamps are predictable.
func newTest(t *testing.T, root string, h *fakeHound, mut func(*Options)) *Orchestrator {
	t.Helper()
	tick := 0
	opts := Options{
		Root:        root,
		Investigate: h.run,
		Report:      fixedReport,
		NewID:       func() (string, error) { return "c-20260728T101500-a1b2c3", nil },
		Now: func() time.Time {
			tick++
			return time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC).Add(time.Duration(tick) * time.Second)
		},
	}
	if mut != nil {
		mut(&opts)
	}
	o, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o
}

// readCheckpoints loads a case's checkpoints or fails the test.
func readCheckpoints(t *testing.T, caseDir string) []Checkpoint {
	t.Helper()
	cps, err := LoadCheckpoints(caseDir)
	if err != nil {
		t.Fatalf("LoadCheckpoints: %v", err)
	}
	return cps
}

func TestHuntWalksPhasesAndCheckpointsThem(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	h := &fakeHound{finding: cannedFinding(), spend: cannedSpend()}

	res, err := newTest(t, root, h, nil).Hunt(t.Context(), alert)
	if err != nil {
		t.Fatalf("Hunt: %v", err)
	}

	if res.Case.Phase != PhaseDone {
		t.Errorf("final phase = %q, want %q", res.Case.Phase, PhaseDone)
	}
	if res.Case.AlertName != "KubePodCrashLooping" {
		t.Errorf("alert name = %q, want it parsed from the alert file", res.Case.AlertName)
	}
	if res.Finding.Summary != cannedFinding().Summary {
		t.Errorf("finding = %+v, want the hound's canned finding", res.Finding)
	}

	// Checkpoints land in walk order with the walk's index in the filename.
	entries, err := os.ReadDir(filepath.Join(res.Case.WorkDir, checkpointDir))
	if err != nil {
		t.Fatalf("reading checkpoint dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"01-intake.json", "02-investigate.json", "03-report.json"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("checkpoint files = %v, want %v", names, want)
	}

	cps := readCheckpoints(t, res.Case.WorkDir)
	for i, cp := range cps {
		if cp.Status != StatusCompleted {
			t.Errorf("checkpoint %d (%s) status = %q, want %q", i, cp.Phase, cp.Status, StatusCompleted)
		}
		if cp.SchemaVersion != CheckpointSchemaVersion {
			t.Errorf("checkpoint %d schema_version = %d, want %d", i, cp.SchemaVersion, CheckpointSchemaVersion)
		}
		if cp.CaseID != res.Case.ID {
			t.Errorf("checkpoint %d case_id = %q, want %q", i, cp.CaseID, res.Case.ID)
		}
		if cp.Error != nil {
			t.Errorf("checkpoint %d error = %v, want null", i, *cp.Error)
		}
		if cp.FinishedAt.Before(cp.StartedAt) {
			t.Errorf("checkpoint %d finished before it started", i)
		}
	}

	// Phase outputs are deserializable as their phase's type.
	var intakeCase Case
	if err := json.Unmarshal(cps[0].Output, &intakeCase); err != nil {
		t.Fatalf("intake output is not a Case: %v", err)
	}
	if intakeCase.AlertName != "KubePodCrashLooping" {
		t.Errorf("intake output alert name = %q", intakeCase.AlertName)
	}
	var finding Finding
	if err := json.Unmarshal(cps[1].Output, &finding); err != nil {
		t.Fatalf("investigate output is not a Finding: %v", err)
	}
	if len(finding.Evidence) != 1 {
		t.Errorf("investigate output evidence = %d records, want 1", len(finding.Evidence))
	}
	var report ReportOutput
	if err := json.Unmarshal(cps[2].Output, &report); err != nil {
		t.Fatalf("report output is not a ReportOutput: %v", err)
	}
	if len(report.Artifacts) != 2 {
		t.Errorf("report artifacts = %v, want report.json and report.txt", report.Artifacts)
	}

	// The work dir layout of spec 002 §4.2 exists in full.
	for _, rel := range []string{
		CaseFile, ReportJSON, ReportText,
		checkpointDir, CaptureDir,
		filepath.Join(CaptureDir, "llm"), filepath.Join(CaptureDir, "mcp"),
	} {
		if _, err := os.Stat(filepath.Join(res.Case.WorkDir, rel)); err != nil {
			t.Errorf("work dir is missing %s: %v", rel, err)
		}
	}

	stored, err := ReadCase(res.Case.WorkDir)
	if err != nil {
		t.Fatalf("ReadCase: %v", err)
	}
	if stored.Phase != PhaseDone {
		t.Errorf("case.json phase = %q, want %q", stored.Phase, PhaseDone)
	}
	if stored.Pipeline != PipelineV0 {
		t.Errorf("case.json pipeline = %q, want %q", stored.Pipeline, PipelineV0)
	}
}

func TestHuntCheckpointLandsBeforeCaseAdvances(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	h := &fakeHound{finding: cannedFinding(), spend: cannedSpend()}

	// Fail the rename of the investigate checkpoint: nothing after it may be
	// observable, and the state before it must still be readable.
	o := newTest(t, root, h, func(opts *Options) {
		opts.rename = func(oldpath, newpath string) error {
			if strings.HasSuffix(newpath, "02-investigate.json") {
				return errors.New("simulated torn write")
			}
			return os.Rename(oldpath, newpath)
		}
	})
	_, err := o.Hunt(t.Context(), alert)
	if err == nil {
		t.Fatal("Hunt succeeded, want the injected rename failure")
	}

	caseDir := filepath.Join(root, "c-20260728T101500-a1b2c3")
	stored, err := ReadCase(caseDir)
	if err != nil {
		t.Fatalf("case.json is unreadable after a torn write: %v", err)
	}
	if stored.Phase != PhaseInvestigate {
		t.Errorf("case.json phase = %q, want it still pointing at %q", stored.Phase, PhaseInvestigate)
	}
	if stored.AlertName != "KubePodCrashLooping" {
		t.Errorf("case.json lost the intake facts: %+v", stored)
	}

	cps := readCheckpoints(t, caseDir)
	if len(cps) != 1 || cps[0].Phase != PhaseIntake {
		t.Fatalf("checkpoints = %+v, want only the completed intake", cps)
	}

	// A failed write leaves no debris for the next run to trip over.
	entries, err := os.ReadDir(filepath.Join(caseDir, checkpointDir))
	if err != nil {
		t.Fatalf("reading checkpoint dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file %s survived the failed rename", e.Name())
		}
	}
}

func TestPhaseFailureRecordsFailedCheckpointAndResumes(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	h := &fakeHound{spend: cannedSpend(), err: errors.New("mcp-prom exited")}

	_, err := newTest(t, root, h, nil).Hunt(t.Context(), alert)
	if err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}
	if !strings.Contains(err.Error(), "mcp-prom exited") {
		t.Errorf("error = %v, want it to carry the hound's failure", err)
	}

	caseDir := filepath.Join(root, "c-20260728T101500-a1b2c3")
	stored, err := ReadCase(caseDir)
	if err != nil {
		t.Fatalf("ReadCase: %v", err)
	}
	if stored.Phase != PhaseFailed {
		t.Errorf("case phase = %q, want %q", stored.Phase, PhaseFailed)
	}

	cps := readCheckpoints(t, caseDir)
	if len(cps) != 2 {
		t.Fatalf("checkpoints = %d, want intake and the failed investigate", len(cps))
	}
	failed := cps[1]
	if failed.Status != StatusFailed {
		t.Errorf("investigate status = %q, want %q", failed.Status, StatusFailed)
	}
	if failed.Error == nil || !strings.Contains(*failed.Error, "mcp-prom exited") {
		t.Errorf("investigate checkpoint error = %v, want the hound's failure", failed.Error)
	}
	if failed.Spend.InputTokens != cannedSpend().InputTokens {
		t.Errorf("failed checkpoint spend = %+v, want the tokens the attempt burned", failed.Spend)
	}
	if _, err := os.Stat(filepath.Join(caseDir, ReportJSON)); !os.IsNotExist(err) {
		t.Error("report.json exists after a failed investigate")
	}

	// The cause is fixed; the case resumes and overwrites the failed
	// checkpoint, without re-running intake.
	intakeStart := cps[0].StartedAt
	fixed := &fakeHound{finding: cannedFinding(), spend: cannedSpend()}
	res, err := newTest(t, root, fixed, nil).Resume(t.Context(), stored.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Case.Phase != PhaseDone {
		t.Errorf("resumed phase = %q, want %q", res.Case.Phase, PhaseDone)
	}

	after := readCheckpoints(t, caseDir)
	if len(after) != 3 {
		t.Fatalf("checkpoints after resume = %d, want 3", len(after))
	}
	if after[0].StartedAt != intakeStart {
		t.Errorf("intake checkpoint was rewritten (%s → %s); it must be loaded, not re-run",
			intakeStart, after[0].StartedAt)
	}
	if after[1].Status != StatusCompleted {
		t.Errorf("investigate status after resume = %q, want the failed checkpoint overwritten", after[1].Status)
	}
	if after[1].Error != nil {
		t.Errorf("investigate error after resume = %v, want null", *after[1].Error)
	}
}

func TestResumeLoadsCompletedPhasesAndContinuesCaptureNumbering(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	caseDir := filepath.Join(root, "c-20260728T101500-a1b2c3")

	// First run: intake and investigate complete, report fails.
	h := &fakeHound{finding: cannedFinding(), spend: cannedSpend(), capture: filepath.Join(caseDir, CaptureDir)}
	o := newTest(t, root, h, func(opts *Options) {
		opts.Report = func(context.Context, ReportInput) ([]Artifact, error) {
			return nil, errors.New("disk full")
		}
	})
	if _, err := o.Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the report failure")
	}
	if h.count() != 1 {
		t.Fatalf("hound calls = %d, want 1", h.count())
	}

	// Second run on the same work dir: intake and investigate are loaded, and
	// the fake hound fails the test if it is called again.
	forbidden := &fakeHound{forbidden: t}
	res, err := newTest(t, root, forbidden, nil).Resume(t.Context(), "c-20260728T101500-a1b2c3")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if forbidden.count() != 0 {
		t.Errorf("investigate ran %d times on resume, want 0", forbidden.count())
	}
	if res.Finding.Summary != cannedFinding().Summary {
		t.Errorf("resumed finding = %+v, want it deserialized from the checkpoint", res.Finding)
	}
	if res.Case.AlertName != "KubePodCrashLooping" {
		t.Errorf("resumed case lost its intake facts: %+v", res.Case)
	}
	if _, err := os.Stat(filepath.Join(caseDir, ReportJSON)); err != nil {
		t.Errorf("report.json missing after resume: %v", err)
	}

	// Capture numbering continues from what the first run wrote rather than
	// restarting at 000 — the §4.3 acceptance property.
	captures, err := os.ReadDir(filepath.Join(caseDir, CaptureDir, "llm"))
	if err != nil {
		t.Fatalf("reading captures: %v", err)
	}
	if len(captures) != 1 || captures[0].Name() != "000-metrics-hound.json" {
		t.Fatalf("captures after resume = %v, want the first run's 000 file untouched", captures)
	}
	if got := nextCaptureSeq(t, filepath.Join(caseDir, CaptureDir, "llm")); got != 1 {
		t.Errorf("next capture sequence = %d, want 1 (continuing, not reset)", got)
	}
}

// nextCaptureSeq reports the sequence number the next capture written into dir
// would take. It calls the same seqname.Next the capture middleware and
// mcpclient call, rather than reimplementing the scan.
//
// It used to reimplement it, with `fmt.Sscanf(name, "%03d-", &n)`, and the
// reimplementation was wrong in exactly the way #10 fixed in the real
// scanners: Sscanf honours the width, so "1000-x.json" scans as n=100 *and*
// returns "input does not match format", and the entry was skipped. A case
// that ran past seq 999 would have made this helper return a number lower than
// the truth and assert the wrong thing while passing.
func nextCaptureSeq(t *testing.T, dir string) int {
	t.Helper()
	next, err := seqname.Next(dir)
	if err != nil {
		t.Fatalf("reading capture dir: %v", err)
	}
	return next
}

// TestCaptureStandInAgreesWithTheSharedFormat is this package's copy of the
// seam the two real capture packages keep: the thing that writes capture names
// here (fakeHound.run) and the thing that reads them back (nextCaptureSeq)
// must agree with each other and with internal/seqname.
//
// The resume tests above only reach seq 1, so nothing here exercised the
// four-digit case that #10 is about, and that is how a hand-rolled Sscanf scan
// sat in this file with #10's bug in it. This is what would notice if either
// half were hand-rolled again.
func TestCaptureStandInAgreesWithTheSharedFormat(t *testing.T) {
	dir := t.TempDir()
	// The names fakeHound.run writes, at the widths #10 cares about.
	for _, seq := range []int{0, 999, 1000} {
		name := seqname.Render(seq, "metrics-hound")
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"seq":0}`), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
	if got := nextCaptureSeq(t, dir); got != 1001 {
		t.Errorf("nextCaptureSeq = %d, want 1001; a fixed-width scan skips 1000-metrics-hound.json and reports a stale number", got)
	}
}

func TestSpendSumsAndCountsTowardBudgetOnResume(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	budget := Budget{MaxTokens: 10_000, MaxToolCalls: 12, MaxWallClock: time.Minute}

	// First attempt burns spend and fails.
	burned := Spend{InputTokens: 4000, OutputTokens: 500, USD: 0.05, ToolCalls: 3}
	h := &fakeHound{spend: burned, err: errors.New("hound crashed")}
	o := newTest(t, root, h, func(opts *Options) { opts.Budget = budget })
	if _, err := o.Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}
	if got := h.lastBudget(t); got.MaxTokens != budget.MaxTokens || got.MaxToolCalls != budget.MaxToolCalls {
		t.Errorf("first budget = %+v, want the full configured budget", got)
	}

	// The resumed attempt is charged for what the failed one already spent.
	second := &fakeHound{finding: cannedFinding(), spend: Spend{InputTokens: 900, OutputTokens: 100, USD: 0.01, ToolCalls: 2}}
	o2 := newTest(t, root, second, func(opts *Options) { opts.Budget = budget })
	res, err := o2.Resume(t.Context(), "c-20260728T101500-a1b2c3")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got := second.lastBudget(t)
	if want := budget.MaxTokens - burned.Tokens(); got.MaxTokens != want {
		t.Errorf("resumed token budget = %d, want %d (prior spend charged)", got.MaxTokens, want)
	}
	if want := budget.MaxToolCalls - burned.ToolCalls; got.MaxToolCalls != want {
		t.Errorf("resumed tool-call budget = %d, want %d", got.MaxToolCalls, want)
	}
	if got.MaxWallClock >= budget.MaxWallClock {
		t.Errorf("resumed wall-clock budget = %s, want less than %s", got.MaxWallClock, budget.MaxWallClock)
	}

	// The result total and `bloodhound cost` agree with the checkpoint sum.
	caseDir := filepath.Join(root, "c-20260728T101500-a1b2c3")
	cps := readCheckpoints(t, caseDir)
	sum := SumSpend(cps)
	wantTokens := burned.Tokens() + second.spend.Tokens()
	// The re-run overwrote the failed investigate checkpoint, so that file now
	// carries both attempts: no spend disappears from the ledger.
	if got := cps[1].Spend.Tokens(); got != wantTokens {
		t.Errorf("investigate checkpoint spend = %d tokens, want %d (both attempts)", got, wantTokens)
	}
	if sum.Tokens() != wantTokens {
		t.Errorf("checkpoint spend sum = %d tokens, want %d", sum.Tokens(), wantTokens)
	}
	if res.Spend.Tokens() != wantTokens {
		t.Errorf("result spend = %d tokens, want %d", res.Spend.Tokens(), wantTokens)
	}
	if _, costCPs, costTotal, err := Cost(root, "c-20260728T101500-a1b2c3"); err != nil {
		t.Errorf("Cost: %v", err)
	} else if costTotal != sum || len(costCPs) != len(cps) {
		t.Errorf("Cost total = %+v over %d checkpoints, want %+v over %d",
			costTotal, len(costCPs), sum, len(cps))
	}
}

func TestBudgetExhaustedByPriorAttemptsFailsThePhase(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	budget := Budget{MaxTokens: 1000, MaxToolCalls: 12, MaxWallClock: time.Minute}

	h := &fakeHound{spend: Spend{InputTokens: 1000}, err: errors.New("hound crashed")}
	if _, err := newTest(t, root, h, func(o *Options) { o.Budget = budget }).Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}

	second := &fakeHound{finding: cannedFinding()}
	_, err := newTest(t, root, second, func(o *Options) { o.Budget = budget }).Resume(t.Context(), "c-20260728T101500-a1b2c3")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Resume error = %v, want ErrBudgetExhausted", err)
	}
	if second.count() != 0 {
		t.Errorf("hound ran %d times on an exhausted budget, want 0", second.count())
	}
}

// phaseClock returns an Options.Now that advances by steps[i] on its i-th call
// and by one second once steps runs out, so a test can give each phase the
// wall-clock duration it needs. A phase's duration is the clock gap between
// the runPhase calls that bracket it: with the walk's calls counted in order,
// steps controls which phase burns how much.
func phaseClock(steps ...time.Duration) func() time.Time {
	base := time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC)
	var i int
	var elapsed time.Duration
	return func() time.Time {
		step := time.Second
		if i < len(steps) {
			step = steps[i]
		}
		i++
		elapsed += step
		return base.Add(elapsed)
	}
}

// wantWallMS asserts the recorded per-phase wall clock, so a test that depends on
// the fake clock's call ordering fails on the fixture rather than on the
// property it is really about.
func wantWallMS(t *testing.T, cps []Checkpoint, phase Phase, want time.Duration) {
	t.Helper()
	for _, cp := range cps {
		if cp.Phase != phase {
			continue
		}
		if got := cp.Spend.WallMS; got != want.Milliseconds() {
			t.Fatalf("%s checkpoint wall clock = %dms, want %dms", phase, got, want.Milliseconds())
		}
		return
	}
	t.Fatalf("no %s checkpoint among %d", phase, len(cps))
}

// TestInvestigateBudgetExcludesIntakeWallClock is issue #17: the hound's budget
// caps one hound run, but wall clock accrues in every phase, so charging it
// from the case total bills the hound for time it never had.
func TestInvestigateBudgetExcludesIntakeWallClock(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	caseID := "c-20260728T101500-a1b2c3"
	budget := Budget{MaxTokens: 10_000, MaxToolCalls: 12, MaxWallClock: 60 * time.Second}

	// A slow intake (20s), then an investigate that burns 5s and fails.
	burned := Spend{InputTokens: 400, OutputTokens: 100, USD: 0.01, ToolCalls: 3}
	first := &fakeHound{spend: burned, err: errors.New("hound crashed")}
	o := newTest(t, root, first, func(opts *Options) {
		opts.Budget = budget
		opts.Now = phaseClock(0, 20*time.Second, 0, 5*time.Second)
	})
	if _, err := o.Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}
	cps := readCheckpoints(t, filepath.Join(root, caseID))
	wantWallMS(t, cps, PhaseIntake, 20*time.Second)
	wantWallMS(t, cps, PhaseInvestigate, 5*time.Second)

	second := &fakeHound{finding: cannedFinding()}
	if _, err := newTest(t, root, second, func(opts *Options) { opts.Budget = budget }).
		Resume(t.Context(), caseID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got := second.lastBudget(t)

	// Only the failed investigate's 5s is charged; intake's 20s is not.
	if want := budget.MaxWallClock - 5*time.Second; got.MaxWallClock != want {
		t.Errorf("resumed wall-clock budget = %s, want %s (intake's 20s must not be charged to the hound)",
			got.MaxWallClock, want)
	}
	// Tokens and tool calls only ever accrue in investigate, so they are
	// charged exactly as before.
	if want := budget.MaxTokens - burned.Tokens(); got.MaxTokens != want {
		t.Errorf("resumed token budget = %d, want %d", got.MaxTokens, want)
	}
	if want := budget.MaxToolCalls - burned.ToolCalls; got.MaxToolCalls != want {
		t.Errorf("resumed tool-call budget = %d, want %d", got.MaxToolCalls, want)
	}
	// The case ledger still carries every phase's spend: the budget is scoped,
	// the accounting is not.
	if got := SumSpend(readCheckpoints(t, filepath.Join(root, caseID))).WallMS; got < (20 * time.Second).Milliseconds() {
		t.Errorf("case total wall clock = %dms, want intake's 20s still counted", got)
	}
}

// reportFirstPipeline is a stand-in for an M2 walk in which a phase that
// records wall clock precedes an investigate re-run — the shape verify →
// replan will have. The v0 walk is linear, so this is the only way to test
// that a non-investigate phase's wall clock stays off the hound's cap from
// both sides.
func reportFirstPipeline() Pipeline {
	return Pipeline{
		Version: "test-report-first",
		Start:   PhaseIntake,
		Next: map[Phase]Phase{
			PhaseIntake:      PhaseReport,
			PhaseReport:      PhaseInvestigate,
			PhaseInvestigate: PhaseDone,
		},
	}
}

func TestInvestigateBudgetExcludesOtherPhasesWallClock(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	caseID := "c-20260728T101500-a1b2c3"
	budget := Budget{MaxTokens: 10_000, MaxToolCalls: 12, MaxWallClock: 60 * time.Second}
	pipeline := reportFirstPipeline()

	// Intake 4s, report 25s (the report phase reads the clock a third time for
	// its timestamp), investigate 3s before the hound fails.
	first := &fakeHound{err: errors.New("hound crashed")}
	o := newTest(t, root, first, func(opts *Options) {
		opts.Budget = budget
		opts.Pipeline = pipeline
		opts.Now = phaseClock(0, 4*time.Second, 0, 25*time.Second, 0, 0, 3*time.Second)
	})
	if _, err := o.Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}
	cps := readCheckpoints(t, filepath.Join(root, caseID))
	wantWallMS(t, cps, PhaseIntake, 4*time.Second)
	wantWallMS(t, cps, PhaseReport, 25*time.Second)
	wantWallMS(t, cps, PhaseInvestigate, 3*time.Second)

	second := &fakeHound{finding: cannedFinding()}
	if _, err := newTest(t, root, second, func(opts *Options) {
		opts.Budget = budget
		opts.Pipeline = pipeline
	}).Resume(t.Context(), caseID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if want := budget.MaxWallClock - 3*time.Second; second.lastBudget(t).MaxWallClock != want {
		t.Errorf("resumed wall-clock budget = %s, want %s (intake's 4s and report's 25s must not be charged to the hound)",
			second.lastBudget(t).MaxWallClock, want)
	}
}

// TestInvestigateBudgetChargesPriorInvestigateAttempts is the other half of
// issue #17: scoping the budget to the phase must not cost the crash-loop
// guard (spec 002 §4.3). Each failed investigate still shrinks the next
// attempt's budget, and enough of them exhaust it.
func TestInvestigateBudgetChargesPriorInvestigateAttempts(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	caseID := "c-20260728T101500-a1b2c3"
	budget := Budget{MaxTokens: 10_000, MaxToolCalls: 12, MaxWallClock: 10 * time.Second}

	// Intake burns far more than the whole hound budget, so charging the case
	// total would exhaust it before the second attempt ever ran; investigate
	// burns 6s per attempt.
	first := &fakeHound{err: errors.New("hound crashed")}
	o := newTest(t, root, first, func(opts *Options) {
		opts.Budget = budget
		opts.Now = phaseClock(0, 30*time.Second, 0, 6*time.Second)
	})
	if _, err := o.Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}
	if got := first.lastBudget(t).MaxWallClock; got != budget.MaxWallClock {
		t.Errorf("first wall-clock budget = %s, want the full %s", got, budget.MaxWallClock)
	}

	// Second attempt: charged for the first attempt's 6s, and only that.
	second := &fakeHound{err: errors.New("hound crashed again")}
	_, err := newTest(t, root, second, func(opts *Options) {
		opts.Budget = budget
		opts.Now = phaseClock(0, 6*time.Second)
	}).Resume(t.Context(), caseID)
	if err == nil {
		t.Fatal("Resume succeeded, want the hound's failure")
	}
	if want := budget.MaxWallClock - 6*time.Second; second.lastBudget(t).MaxWallClock != want {
		t.Fatalf("second wall-clock budget = %s, want %s (prior investigate spend must be charged)",
			second.lastBudget(t).MaxWallClock, want)
	}

	// Third attempt: 12s of investigate wall clock against a 10s cap is the
	// crash loop stopping.
	third := &fakeHound{finding: cannedFinding()}
	_, err = newTest(t, root, third, func(opts *Options) { opts.Budget = budget }).Resume(t.Context(), caseID)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("third Resume error = %v, want ErrBudgetExhausted", err)
	}
	if third.count() != 0 {
		t.Errorf("hound ran %d times on an exhausted budget, want 0", third.count())
	}
}

func TestPhaseTimeoutWritesFailedCheckpointAndCaseResumes(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)

	slow := &fakeHound{finding: cannedFinding(), delay: 30 * time.Second}
	o := newTest(t, root, slow, func(opts *Options) {
		opts.timeout = func(phase Phase) time.Duration {
			if phase == PhaseInvestigate {
				return 20 * time.Millisecond
			}
			return 10 * time.Second
		}
	})
	_, err := o.Hunt(t.Context(), alert)
	if err == nil {
		t.Fatal("Hunt succeeded, want the phase timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}

	caseDir := filepath.Join(root, "c-20260728T101500-a1b2c3")
	cps := readCheckpoints(t, caseDir)
	failed := cps[len(cps)-1]
	if failed.Phase != PhaseInvestigate || failed.Status != StatusFailed {
		t.Fatalf("last checkpoint = %s/%s, want a failed investigate", failed.Phase, failed.Status)
	}
	if failed.Error == nil || !strings.Contains(*failed.Error, "deadline exceeded") {
		t.Errorf("checkpoint error = %v, want the deadline error", failed.Error)
	}
	if stored, err := ReadCase(caseDir); err != nil || stored.Phase != PhaseFailed {
		t.Errorf("case phase = %v (err %v), want %q", stored.Phase, err, PhaseFailed)
	}

	// A timed-out case is still resumable.
	fast := &fakeHound{finding: cannedFinding(), spend: cannedSpend()}
	res, err := newTest(t, root, fast, nil).Resume(t.Context(), "c-20260728T101500-a1b2c3")
	if err != nil {
		t.Fatalf("Resume after timeout: %v", err)
	}
	if res.Case.Phase != PhaseDone {
		t.Errorf("resumed phase = %q, want %q", res.Case.Phase, PhaseDone)
	}
}

func TestHuntBadAlertFileIsResumableAfterTheFix(t *testing.T) {
	root := t.TempDir()
	alertDir := t.TempDir()
	bad := writeAlert(t, alertDir, `{"alerts": [{"labels": {}}]}`)
	h := &fakeHound{finding: cannedFinding(), spend: cannedSpend()}

	_, err := newTest(t, root, h, nil).Hunt(t.Context(), bad)
	if !errors.Is(err, ErrBadAlert) {
		t.Fatalf("Hunt error = %v, want ErrBadAlert", err)
	}

	caseDir := filepath.Join(root, "c-20260728T101500-a1b2c3")
	cps := readCheckpoints(t, caseDir)
	if len(cps) != 1 || cps[0].Phase != PhaseIntake || cps[0].Status != StatusFailed {
		t.Fatalf("checkpoints = %+v, want a single failed intake", cps)
	}

	// Fix the alert file in place; the case resumes from the same work dir.
	if err := os.WriteFile(bad, []byte(alertFixture), 0o644); err != nil {
		t.Fatalf("rewriting alert: %v", err)
	}
	res, err := newTest(t, root, h, nil).Resume(t.Context(), "c-20260728T101500-a1b2c3")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Case.AlertName != "KubePodCrashLooping" {
		t.Errorf("alert name = %q, want the corrected alert parsed", res.Case.AlertName)
	}
}

func TestHuntMissingAlertFile(t *testing.T) {
	root := t.TempDir()
	h := &fakeHound{finding: cannedFinding()}
	_, err := newTest(t, root, h, nil).Hunt(t.Context(), filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, ErrBadAlert) {
		t.Fatalf("Hunt error = %v, want ErrBadAlert", err)
	}
}

// v1Pipeline is a stand-in for a future walk: same phases as V0 so that New
// accepts it (M2's plan phase has no implementation yet), but a different
// version. The guard under test compares versions, not tables.
func v1Pipeline() Pipeline {
	p := V0()
	p.Version = "v1"
	return p
}

// dirState is a snapshot of every entry under dir, keyed by work-dir-relative
// path. Files record their mode, size, modification time and content hash;
// directories record their modification time, so a directory created or an
// entry added under one shows up as a difference too.
func dirState(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			state[rel] = fmt.Sprintf("dir mode=%s mtime=%d", info.Mode(), info.ModTime().UnixNano())
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		state[rel] = fmt.Sprintf("file mode=%s size=%d mtime=%d sha256=%x",
			info.Mode(), info.Size(), info.ModTime().UnixNano(), sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", dir, err)
	}
	return state
}

// assertDirUnchanged fails the test with the specific paths that differ.
func assertDirUnchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	for rel, want := range before {
		got, ok := after[rel]
		if !ok {
			t.Errorf("work dir entry %s disappeared", rel)
			continue
		}
		if got != want {
			t.Errorf("work dir entry %s changed:\n before: %s\n  after: %s", rel, want, got)
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			t.Errorf("work dir entry %s was created", rel)
		}
	}
}

// TestResumeRefusesPipelineVersionMismatch is issue #16: a case written under
// one pipeline must not be walked by an orchestrator holding another. The
// checkpoint filenames carry the walk index, so a shifted index would orphan a
// checkpoint and make SumSpend double-count the phase it recorded.
func TestResumeRefusesPipelineVersionMismatch(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	caseDir := filepath.Join(root, "c-20260728T101500-a1b2c3")

	// A v0 case that failed mid-walk: exactly the state an operator resumes.
	crashed := &fakeHound{spend: cannedSpend(), err: errors.New("hound crashed")}
	if _, err := newTest(t, root, crashed, nil).Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}
	if stored, err := ReadCase(caseDir); err != nil {
		t.Fatalf("ReadCase: %v", err)
	} else if stored.Pipeline != PipelineV0 {
		t.Fatalf("case pipeline = %q, want %q", stored.Pipeline, PipelineV0)
	}

	// Remove one skeleton directory first: opening the store would recreate
	// it, so its continued absence is what proves the refusal happens before
	// the work dir is touched at all.
	if err := os.Remove(filepath.Join(caseDir, CaptureDir, "mcp")); err != nil {
		t.Fatalf("removing capture dir: %v", err)
	}
	before := dirState(t, caseDir)

	forbidden := &fakeHound{forbidden: t}
	o := newTest(t, root, forbidden, func(opts *Options) { opts.Pipeline = v1Pipeline() })
	_, err := o.Resume(t.Context(), "c-20260728T101500-a1b2c3")
	if !errors.Is(err, ErrPipelineMismatch) {
		t.Fatalf("Resume error = %v, want ErrPipelineMismatch", err)
	}
	for _, want := range []string{"c-20260728T101500-a1b2c3", `"v0"`, `"v1"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err, want)
		}
	}
	if forbidden.count() != 0 {
		t.Errorf("investigate ran %d times on a refused resume, want 0", forbidden.count())
	}

	assertDirUnchanged(t, before, dirState(t, caseDir))

	// Nothing was added to the ledger either: `bloodhound cost` still sees the
	// two checkpoints the v0 run wrote, and no third.
	cps := readCheckpoints(t, caseDir)
	if len(cps) != 2 {
		t.Errorf("checkpoints after a refused resume = %d, want the 2 the v0 run wrote", len(cps))
	}
}

// TestResumeUnderMatchingNonDefaultPipeline pins that the guard compares the
// case against the orchestrator's own version rather than hard-coding v0.
func TestResumeUnderMatchingNonDefaultPipeline(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	v1 := func(opts *Options) { opts.Pipeline = v1Pipeline() }

	crashed := &fakeHound{spend: cannedSpend(), err: errors.New("hound crashed")}
	if _, err := newTest(t, root, crashed, v1).Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}

	fixed := &fakeHound{finding: cannedFinding(), spend: cannedSpend()}
	res, err := newTest(t, root, fixed, v1).Resume(t.Context(), "c-20260728T101500-a1b2c3")
	if err != nil {
		t.Fatalf("Resume under a matching v1 pipeline: %v", err)
	}
	if res.Case.Phase != PhaseDone {
		t.Errorf("resumed phase = %q, want %q", res.Case.Phase, PhaseDone)
	}
	if res.Case.Pipeline != "v1" {
		t.Errorf("resumed case pipeline = %q, want %q", res.Case.Pipeline, "v1")
	}
	if fixed.count() != 1 {
		t.Errorf("hound ran %d times, want 1 (the failed investigate re-run)", fixed.count())
	}
}

// TestResumeCaseWithNoRecordedPipeline covers a case file of unknown
// provenance: an absent version is a mismatch, not an implicit v0.
func TestResumeCaseWithNoRecordedPipeline(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	caseDir := filepath.Join(root, "c-20260728T101500-a1b2c3")

	crashed := &fakeHound{spend: cannedSpend(), err: errors.New("hound crashed")}
	if _, err := newTest(t, root, crashed, nil).Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}
	stored, err := ReadCase(caseDir)
	if err != nil {
		t.Fatalf("ReadCase: %v", err)
	}
	stored.Pipeline = ""
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		t.Fatalf("marshaling case: %v", err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, CaseFile), append(data, '\n'), 0o644); err != nil {
		t.Fatalf("rewriting case file: %v", err)
	}

	forbidden := &fakeHound{forbidden: t}
	if _, err := newTest(t, root, forbidden, nil).Resume(t.Context(), "c-20260728T101500-a1b2c3"); !errors.Is(err, ErrPipelineMismatch) {
		t.Fatalf("Resume error = %v, want ErrPipelineMismatch", err)
	}
}

func TestResumeUnknownCase(t *testing.T) {
	h := &fakeHound{finding: cannedFinding()}
	if _, err := newTest(t, t.TempDir(), h, nil).Resume(t.Context(), "c-nope"); err == nil {
		t.Fatal("Resume succeeded on a case that was never opened")
	}
}

func TestNewRejectsIncompleteOptions(t *testing.T) {
	h := &fakeHound{}
	tests := map[string]Options{
		"no root":        {Investigate: h.run, Report: fixedReport},
		"no investigate": {Root: "work", Report: fixedReport},
		"no report":      {Root: "work", Investigate: h.run},
		"unimplemented phase": {
			Root: "work", Investigate: h.run, Report: fixedReport,
			Pipeline: Pipeline{Version: "vx", Start: PhasePlan, Next: map[Phase]Phase{PhasePlan: PhaseDone}},
		},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); err == nil {
				t.Fatal("New succeeded, want a validation error")
			}
		})
	}
}

func TestNewCaseIDFormat(t *testing.T) {
	id, err := NewCaseID(time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewCaseID: %v", err)
	}
	const prefix = "c-20260728T101500-"
	if !strings.HasPrefix(id, prefix) {
		t.Fatalf("id = %q, want prefix %q", id, prefix)
	}
	suffix := strings.TrimPrefix(id, prefix)
	if len(suffix) != 6 {
		t.Errorf("random suffix = %q, want 6 hex characters", suffix)
	}
	if strings.ContainsFunc(suffix, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) {
		t.Errorf("random suffix = %q, want lowercase hex", suffix)
	}

	other, err := NewCaseID(time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewCaseID: %v", err)
	}
	if other == id {
		t.Error("two IDs minted in the same second collided")
	}
}

func TestRemainingBudget(t *testing.T) {
	base := Budget{MaxTokens: 1000, MaxToolCalls: 10, MaxWallClock: time.Minute}
	tests := []struct {
		name    string
		spent   Spend
		want    Budget
		wantErr bool
	}{
		{
			name:  "nothing spent",
			spent: Spend{},
			want:  base,
		},
		{
			name:  "partial spend",
			spent: Spend{InputTokens: 200, OutputTokens: 100, ToolCalls: 4, WallMS: 15_000},
			want:  Budget{MaxTokens: 700, MaxToolCalls: 6, MaxWallClock: 45 * time.Second},
		},
		{
			name:    "tokens exhausted",
			spent:   Spend{InputTokens: 1000},
			wantErr: true,
		},
		{
			name:    "tool calls exhausted",
			spent:   Spend{ToolCalls: 10},
			wantErr: true,
		},
		{
			name:    "wall clock exhausted",
			spent:   Spend{WallMS: 60_000},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := remainingBudget(base, tt.spent)
			if tt.wantErr {
				if !errors.Is(err, ErrBudgetExhausted) {
					t.Fatalf("error = %v, want ErrBudgetExhausted", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("remainingBudget: %v", err)
			}
			if got != tt.want {
				t.Errorf("remaining = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPhaseTimeoutsFollowTheSpec(t *testing.T) {
	h := &fakeHound{}
	o, err := New(Options{Root: "work", Investigate: h.run, Report: fixedReport, Budget: Budget{MaxWallClock: 2 * time.Minute}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := map[Phase]time.Duration{
		PhaseIntake:      IntakeTimeout,
		PhaseInvestigate: 2*time.Minute + InvestigateGrace,
		PhaseReport:      ReportTimeout,
	}
	for phase, want := range tests {
		if got := o.phaseTimeout(phase, Spend{}); got != want {
			t.Errorf("%s timeout = %s, want %s", phase, got, want)
		}
	}
}

// TestInvestigatePhaseTimeoutUsesRemainingBudget is the second half of issue
// #32: the deadline must mean what it says. A resume with four seconds of
// wall-clock budget left may not be handed the deadline a fresh case gets.
func TestInvestigatePhaseTimeoutUsesRemainingBudget(t *testing.T) {
	h := &fakeHound{}
	o, err := New(Options{Root: "work", Investigate: h.run, Report: fixedReport,
		Budget: Budget{MaxWallClock: 2 * time.Minute}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := []struct {
		name  string
		spent Spend
		want  time.Duration
	}{
		{name: "fresh case", spent: Spend{}, want: 2*time.Minute + InvestigateGrace},
		{name: "half spent", spent: Spend{WallMS: 60_000}, want: time.Minute + InvestigateGrace},
		{name: "four seconds left", spent: Spend{WallMS: 116_000}, want: 4*time.Second + InvestigateGrace},
		// An overspent phase is refused by the budget check; the deadline is
		// clamped so the refusal is not reported as a timeout.
		{name: "overspent", spent: Spend{WallMS: 200_000}, want: InvestigateGrace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := o.phaseTimeout(PhaseInvestigate, tt.spent); got != tt.want {
				t.Errorf("investigate timeout = %s, want %s", got, tt.want)
			}
		})
	}

	// A disabled cap has no remainder to compute and keeps the default
	// backstop, whatever the phase has already spent.
	disabled, err := New(Options{Root: "work", Investigate: h.run, Report: fixedReport,
		Budget: Budget{MaxWallClock: -1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := disabled.phaseTimeout(PhaseInvestigate, Spend{WallMS: 999_000}); got != DefaultMaxWallClock+InvestigateGrace {
		t.Errorf("disabled-cap timeout = %s, want %s", got, DefaultMaxWallClock+InvestigateGrace)
	}
}

// TestResumedInvestigateDeadlineReflectsRemainingBudget is the same property
// observed from inside the phase: the context the hound actually runs under
// carries the shortened deadline, not just the number phaseTimeout computes.
func TestResumedInvestigateDeadlineReflectsRemainingBudget(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	caseID := "c-20260728T101500-a1b2c3"
	budget := Budget{MaxTokens: 10_000, MaxToolCalls: 12, MaxWallClock: 60 * time.Second}

	// A first attempt that burns 56s of the 60s cap and fails.
	first := &fakeHound{err: errors.New("hound crashed")}
	if _, err := newTest(t, root, first, func(o *Options) {
		o.Budget = budget
		o.Now = phaseClock(0, time.Second, 0, 56*time.Second)
	}).Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}
	wantWallMS(t, readCheckpoints(t, filepath.Join(root, caseID)), PhaseInvestigate, 56*time.Second)

	// The resumed attempt has 4s of budget left, so its deadline is 4s plus the
	// grace window — not the 90s a fresh case would get. The deadline comes off
	// the real clock, so this is a bound rather than an equality.
	var left time.Duration
	second := &fakeHound{finding: cannedFinding()}
	o := newTest(t, root, second, func(opts *Options) {
		opts.Budget = budget
		opts.Investigate = func(ctx context.Context, c Case, b Budget) (Finding, Spend, error) {
			if dl, ok := ctx.Deadline(); ok {
				left = time.Until(dl)
			}
			return second.run(ctx, c, b)
		}
	})
	if _, err := o.Resume(t.Context(), caseID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if want := 4*time.Second + InvestigateGrace; left > want || left < want-5*time.Second {
		t.Errorf("investigate deadline = %s away, want about %s (4s of budget left, not the full %s)",
			left, want, budget.MaxWallClock+InvestigateGrace)
	}
}

// TestRefusedResumeDoesNotInflateTheLedger is the first half of issue #32: a
// resume refused by the budget check never runs the hound, so the attempt cost
// the case nothing. Recording its measured wall clock would make the ledger
// `bloodhound cost` reads climb with every futile retry.
func TestRefusedResumeDoesNotInflateTheLedger(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	caseID := "c-20260728T101500-a1b2c3"
	caseDir := filepath.Join(root, caseID)
	budget := Budget{MaxTokens: 1000, MaxToolCalls: 12, MaxWallClock: time.Minute}

	// One genuine attempt that burns the whole token cap and dies.
	h := &fakeHound{spend: Spend{InputTokens: 1000}, err: errors.New("hound crashed")}
	if _, err := newTest(t, root, h, func(o *Options) { o.Budget = budget }).Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}
	want := SumSpend(readCheckpoints(t, caseDir))
	if want.WallMS == 0 {
		t.Fatal("the first attempt recorded no wall clock; the fixture cannot detect inflation")
	}

	// Three futile resumes: the recorded spend must be stable across all of
	// them, not merely correct after the first.
	for attempt := 1; attempt <= 3; attempt++ {
		refused := &fakeHound{finding: cannedFinding()}
		_, err := newTest(t, root, refused, func(o *Options) { o.Budget = budget }).Resume(t.Context(), caseID)
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("resume %d error = %v, want ErrBudgetExhausted", attempt, err)
		}
		if refused.count() != 0 {
			t.Fatalf("resume %d ran the hound %d times, want 0", attempt, refused.count())
		}
		if got := SumSpend(readCheckpoints(t, caseDir)); got != want {
			t.Errorf("ledger after %d refused resume(s) = %+v, want it stable at %+v", attempt, got, want)
		}
		if _, _, cost, err := Cost(root, caseID); err != nil {
			t.Fatalf("Cost after %d refused resume(s): %v", attempt, err)
		} else if cost != want {
			t.Errorf("bloodhound cost after %d refused resume(s) = %+v, want %+v", attempt, cost, want)
		}
	}
}

// investigateWallMS returns the wall clock recorded for the investigate phase.
func investigateWallMS(t *testing.T, cps []Checkpoint) int64 {
	t.Helper()
	for _, cp := range cps {
		if cp.Phase == PhaseInvestigate {
			return cp.Spend.WallMS
		}
	}
	t.Fatalf("no investigate checkpoint among %d", len(cps))
	return 0
}

// TestCrashLoopStillChargesEveryAttemptThatRan guards #17's guarantee (spec
// 002 §4.3) against the issue #32 fix, which moves in the opposite direction by
// recording less: only an attempt that never ran is free. Every attempt that
// does run still grows the ledger, and enough of them exhaust the budget.
func TestCrashLoopStillChargesEveryAttemptThatRan(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	caseID := "c-20260728T101500-a1b2c3"
	caseDir := filepath.Join(root, caseID)
	budget := Budget{MaxTokens: 10_000, MaxToolCalls: 12, MaxWallClock: 10 * time.Second}

	// Intake burns 30s — more than the whole hound cap — so a case-total charge
	// would exhaust the budget before the hound ever ran. Each investigate
	// attempt burns 4s and dies.
	first := &fakeHound{err: errors.New("hound crashed")}
	if _, err := newTest(t, root, first, func(o *Options) {
		o.Budget = budget
		o.Now = phaseClock(0, 30*time.Second, 0, 4*time.Second)
	}).Hunt(t.Context(), alert); err == nil {
		t.Fatal("Hunt succeeded, want the hound's failure")
	}
	if first.count() != 1 {
		t.Fatalf("first attempt ran the hound %d times, want 1", first.count())
	}
	recorded := []int64{investigateWallMS(t, readCheckpoints(t, caseDir))}

	const maxAttempts = 8
	for attempt := 2; ; attempt++ {
		if attempt > maxAttempts {
			t.Fatalf("crash loop never converged after %d attempts: investigate wall clock %v",
				maxAttempts, recorded)
		}
		h := &fakeHound{err: fmt.Errorf("hound crashed on attempt %d", attempt)}
		_, err := newTest(t, root, h, func(o *Options) {
			o.Budget = budget
			o.Now = phaseClock(0, 4*time.Second)
		}).Resume(t.Context(), caseID)
		if err == nil {
			t.Fatalf("attempt %d succeeded, want the hound's failure", attempt)
		}
		got := investigateWallMS(t, readCheckpoints(t, caseDir))
		last := recorded[len(recorded)-1]

		if errors.Is(err, ErrBudgetExhausted) {
			if h.count() != 0 {
				t.Errorf("hound ran %d times on an exhausted budget, want 0", h.count())
			}
			if got != last {
				t.Errorf("refused attempt %d changed the recorded wall clock %dms → %dms", attempt, last, got)
			}
			if want := budget.MaxWallClock.Milliseconds(); got <= want {
				t.Errorf("wall clock at convergence = %dms, want more than the %dms cap", got, want)
			}
			break
		}
		if h.count() != 1 {
			t.Fatalf("attempt %d ran the hound %d times, want 1", attempt, h.count())
		}
		if got <= last {
			t.Fatalf("attempt %d ran but the ledger did not grow: %dms → %dms (prior investigate spend must still be charged)",
				attempt, last, got)
		}
		recorded = append(recorded, got)
	}
	if len(recorded) < 2 {
		t.Fatalf("only %d attempt(s) ran before convergence; the fixture proves nothing about accumulation", len(recorded))
	}
}

// TestSpendFooterAndCostReportEveryPhase pins that scoping the hound's budget
// and the ledger fix leave the *reporting* path whole: Result.Spend, the spend
// the reporter renders into its footer, and `bloodhound cost` all carry the
// case total across every phase, not just investigate.
func TestSpendFooterAndCostReportEveryPhase(t *testing.T) {
	root := t.TempDir()
	alert := writeAlert(t, t.TempDir(), alertFixture)
	caseID := "c-20260728T101500-a1b2c3"
	caseDir := filepath.Join(root, caseID)

	var footer Spend
	h := &fakeHound{finding: cannedFinding(), spend: cannedSpend()}
	// Intake 7s, investigate 5s, report 3s: every phase contributes wall clock.
	res, err := newTest(t, root, h, func(opts *Options) {
		opts.Now = phaseClock(0, 7*time.Second, 0, 5*time.Second, 0, 0, 3*time.Second)
		opts.Report = func(ctx context.Context, in ReportInput) ([]Artifact, error) {
			footer = in.Spend
			return fixedReport(ctx, in)
		}
	}).Hunt(t.Context(), alert)
	if err != nil {
		t.Fatalf("Hunt: %v", err)
	}

	cps := readCheckpoints(t, caseDir)
	wantWallMS(t, cps, PhaseIntake, 7*time.Second)
	wantWallMS(t, cps, PhaseInvestigate, 5*time.Second)
	wantWallMS(t, cps, PhaseReport, 3*time.Second)

	sum := SumSpend(cps)
	if want := (15 * time.Second).Milliseconds(); sum.WallMS != want {
		t.Errorf("checkpoint sum wall clock = %dms, want %dms (7s + 5s + 3s)", sum.WallMS, want)
	}
	if res.Spend != sum {
		t.Errorf("Result.Spend = %+v, want the checkpoint sum %+v", res.Spend, sum)
	}
	// The footer is rendered from inside the report phase, before its own
	// checkpoint exists, so it carries every phase that had finished by then.
	if want := (12 * time.Second).Milliseconds(); footer.WallMS != want {
		t.Errorf("report spend footer wall clock = %dms, want %dms (intake 7s + investigate 5s)", footer.WallMS, want)
	}
	if footer.Tokens() != cannedSpend().Tokens() || footer.ToolCalls != cannedSpend().ToolCalls {
		t.Errorf("report spend footer = %+v, want the hound's tokens and tool calls", footer)
	}
	if _, costCPs, cost, err := Cost(root, caseID); err != nil {
		t.Fatalf("Cost: %v", err)
	} else if cost != sum || len(costCPs) != len(cps) {
		t.Errorf("bloodhound cost = %+v over %d checkpoints, want %+v over %d",
			cost, len(costCPs), sum, len(cps))
	}
}
