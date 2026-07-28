package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
		name := fmt.Sprintf("%03d-metrics-hound.json", n-1)
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
// would take, mirroring the scan the capture middleware and mcpclient do.
func nextCaptureSeq(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading capture dir: %v", err)
	}
	next := 0
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "%03d-", &n); err != nil {
			continue
		}
		if n+1 > next {
			next = n + 1
		}
	}
	return next
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
		if got := o.phaseTimeout(phase); got != want {
			t.Errorf("%s timeout = %s, want %s", phase, got, want)
		}
	}
}
