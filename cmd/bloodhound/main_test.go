package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// update rewrites the golden files instead of comparing against them:
// go test ./cmd/bloodhound -update
var update = flag.Bool("update", false, "rewrite golden files")

// checkGolden compares got against testdata/name, or rewrites it under -update.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("creating testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden (run with -update to create it): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s does not match the golden file.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// stubApp builds an app whose model provider and MCP session never run: the
// commands under test fail before reaching them, or do not need them.
func stubApp(t *testing.T) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, func(string) string { return "" })
	a.now = func() time.Time { return time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC) }
	a.newProvider = func(providerConfig) (provider, error) {
		t.Error("the model provider was built when the command should not have needed it")
		return nil, errors.New("unexpected provider")
	}
	a.newSession = func(context.Context, sessionConfig) (toolSession, error) {
		t.Error("an MCP session was opened when the command should not have needed one")
		return nil, errors.New("unexpected session")
	}
	return a, &stdout, &stderr
}

func TestExitCodes(t *testing.T) {
	work := t.TempDir()
	badAlert := filepath.Join(t.TempDir(), "alert.json")
	if err := os.WriteFile(badAlert, []byte(`{"alerts":[]}`), 0o644); err != nil {
		t.Fatalf("writing bad alert: %v", err)
	}

	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "version", args: []string{"version"}, want: 0},
		{name: "help", args: []string{"--help"}, want: 0},
		{name: "no command", args: nil, want: 2},
		{name: "unknown command", args: []string{"sniff"}, want: 2},
		{name: "hunt with no target", args: []string{"hunt"}, want: 2},
		{name: "hunt with both targets", args: []string{"hunt", "--alert", "a.json", "--resume", "c-1"}, want: 2},
		{name: "hunt with a stray argument", args: []string{"hunt", "--alert", "a.json", "extra"}, want: 2},
		{name: "hunt with an unknown flag", args: []string{"hunt", "--sniff"}, want: 2},
		{name: "missing alert file", args: []string{"hunt", "--work", work, "--alert", filepath.Join(work, "nope.json")}, want: 2},
		{name: "unusable alert file", args: []string{"hunt", "--work", work, "--alert", badAlert}, want: 2},
		{name: "cost with no case", args: []string{"cost"}, want: 2},
		// The five below changed from want: 1 in issue #45. Each is a refusal
		// the CLI used to report as a fault, so a wrapper retrying exit 1
		// looped on a command that could not succeed. The dedicated tests
		// under this one carry the reasoning per path.
		{name: "cost for an unknown case", args: []string{"cost", "--work", work, "c-nope"}, want: 2},
		{name: "resume an unknown case", args: []string{"hunt", "--work", work, "--resume", "c-nope"}, want: 2},
		{name: "hunt with an empty work root", args: []string{"hunt", "--work", "", "--alert", badAlert}, want: 2},
		{name: "serve is M2", args: []string{"serve"}, want: 2},
		{name: "replay is M2", args: []string{"replay", "c-1"}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _, _ := stubApp(t)
			if got := exitCode(a.run(t.Context(), tt.args)); got != tt.want {
				t.Errorf("exit code = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestBadAlertFileExitsTwoButLeavesAResumableCase pins the two halves of the
// contract that make exit code 2 meaningful: the invocation is rejected, and
// the case it opened is still there to resume once the file is fixed.
func TestBadAlertFileExitsTwoButLeavesAResumableCase(t *testing.T) {
	work := t.TempDir()
	alert := filepath.Join(t.TempDir(), "alert.json")
	if err := os.WriteFile(alert, []byte(`{"alerts":[{"labels":{}}]}`), 0o644); err != nil {
		t.Fatalf("writing alert: %v", err)
	}

	a, _, stderr := stubApp(t)
	err := a.run(t.Context(), []string{"hunt", "--work", work, "--alert", alert})
	if !errors.Is(err, orchestrator.ErrBadAlert) {
		t.Fatalf("error = %v, want ErrBadAlert", err)
	}
	if exitCode(err) != 2 {
		t.Errorf("exit code = %d, want 2", exitCode(err))
	}
	if stderr.Len() != 0 {
		t.Logf("stderr: %s", stderr)
	}

	dirs := caseDirs(t, work)
	if len(dirs) != 1 {
		t.Fatalf("case dirs = %v, want the failed case to be on disk", dirs)
	}
	if got := readCase(t, dirs[0]); got.Phase != orchestrator.PhaseFailed {
		t.Errorf("case phase = %q, want %q", got.Phase, orchestrator.PhaseFailed)
	}
}

// TestPipelineMismatchResumeExitsTwo pins issue #24: a resume the orchestrator
// refuses because the case records a different pipeline version exits 2, not
// 1. Exit 1 is the arm a retrying operator wrapper keys on, and this refusal
// has deliberately no migration path, so reporting it as 1 would make such a
// wrapper loop on a case that can never be resumed.
func TestPipelineMismatchResumeExitsTwo(t *testing.T) {
	work := t.TempDir()
	caseID := "c-20260728T101500-a1b2c3"
	caseDir := filepath.Join(work, caseID)
	writeCaseFixture(t, caseDir, caseID)

	// Rewrite the case file under a pipeline version this binary does not
	// walk: the state an operator is left holding after a pipeline bump.
	c := readCase(t, caseDir)
	c.Pipeline = "v-from-the-future"
	casePath := filepath.Join(caseDir, orchestrator.CaseFile)
	writeJSONFixture(t, casePath, c)
	before, err := os.ReadFile(casePath)
	if err != nil {
		t.Fatalf("reading case file: %v", err)
	}

	a, _, _ := stubApp(t)
	runErr := a.run(t.Context(), []string{"hunt", "--work", work, "--resume", caseID})
	if !errors.Is(runErr, orchestrator.ErrPipelineMismatch) {
		t.Fatalf("error = %v, want ErrPipelineMismatch", runErr)
	}
	if got := exitCode(runErr); got != 2 {
		t.Errorf("exit code = %d, want 2; exit 1 is the arm a retrying wrapper keys on, and this case can never be resumed", got)
	}
	// case.json is byte-identical afterwards: the refusal did not advance the
	// case, so there is nothing an operator could fix and retry. This checks
	// only the one file. That the whole work dir is untouched — no store
	// skeleton recreated, no checkpoint written — is pinned a layer down by
	// orchestrator.TestResumeRefusesPipelineVersionMismatch, which compares
	// full directory state.
	after, err := os.ReadFile(casePath)
	if err != nil {
		t.Fatalf("reading case file after the refusal: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Errorf("case file changed on a refused resume:\n--- got ---\n%s\n--- want ---\n%s", after, before)
	}
}

// TestUnimplementedCommandsExitTwo pins the purest recurrence of issue #24:
// serve and replay do not exist until M2, so a wrapper that retries exit 1
// would retry them for a whole milestone. Nothing ran and nothing was written,
// and no rerun of this binary can change that — only a newer binary can.
func TestUnimplementedCommandsExitTwo(t *testing.T) {
	for _, args := range [][]string{{"serve"}, {"replay", "c-1"}} {
		t.Run(args[0], func(t *testing.T) {
			a, stdout, _ := stubApp(t)
			err := a.run(t.Context(), args)
			if !errors.Is(err, errNotImplemented) {
				t.Fatalf("error = %v, want errNotImplemented", err)
			}
			if got := exitCode(err); got != 2 {
				t.Errorf("exit code = %d, want 2; exit 1 is the fault arm and this command cannot succeed until M2", got)
			}
			// Not errUsage: the invocation is right, it is just early. A
			// wrapper reading stderr must not be told to fix the command.
			if errors.Is(err, errUsage) {
				t.Error("an unimplemented command reported itself as a bad invocation")
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing written", stdout)
			}
		})
	}
}

// TestEmptyWorkRootExitsTwo pins the --work "" arm. It is a bad invocation
// caught by orchestrator.New, which returned an ordinary error and so fell out
// as exit 1 before issue #45. cost is checked too: without the CLI-side check
// it resolves the case dir against the process cwd instead of refusing.
func TestEmptyWorkRootExitsTwo(t *testing.T) {
	alert := filepath.Join(t.TempDir(), "alert.json")
	if err := os.WriteFile(alert, []byte(`{"alerts":[]}`), 0o644); err != nil {
		t.Fatalf("writing alert: %v", err)
	}
	cmds := map[string][]string{
		"hunt": {"hunt", "--work", "", "--alert", alert},
		"cost": {"cost", "--work", "", "c-1"},
	}
	for name, args := range cmds {
		t.Run(name, func(t *testing.T) {
			a, _, _ := stubApp(t)
			err := a.run(t.Context(), args)
			if !errors.Is(err, errUsage) {
				t.Fatalf("error = %v, want errUsage", err)
			}
			if got := exitCode(err); got != 2 {
				t.Errorf("exit code = %d, want 2; an empty --work is a bad invocation, not a fault", got)
			}
		})
	}
}

// TestMissingCaseExitsTwo pins the "no such case" half of the resume/cost
// decision (issue #45): a typo'd case ID is fix-and-retry, which is exit 2's
// contract — the ID has to change, and rerunning as-is finds the same absence
// forever. It must report ErrNoSuchCase and not ErrCorruptCase, because the two
// tell an operator to do different things.
func TestMissingCaseExitsTwo(t *testing.T) {
	work := t.TempDir()
	cmds := map[string][]string{
		"resume": {"hunt", "--work", work, "--resume", "c-nope"},
		"cost":   {"cost", "--work", work, "c-nope"},
	}
	for name, args := range cmds {
		t.Run(name, func(t *testing.T) {
			a, _, _ := stubApp(t)
			err := a.run(t.Context(), args)
			if !errors.Is(err, orchestrator.ErrNoSuchCase) {
				t.Fatalf("error = %v, want ErrNoSuchCase", err)
			}
			if errors.Is(err, orchestrator.ErrCorruptCase) {
				t.Error("an absent case reported itself as corrupt; the two need different operator actions")
			}
			if got := exitCode(err); got != 2 {
				t.Errorf("exit code = %d, want 2; there is no case, so there is nothing a retry could resume", got)
			}
		})
	}
}

// TestCorruptCaseExitsTwo pins the other half: on-disk state this binary cannot
// decode. The bytes do not change between attempts, so exit 1 would loop a
// retrying wrapper forever on a work dir truncated by a full disk — the exact
// failure PR #40 predicted and deferred. Every form of undecodable state counts,
// not just case.json: a corrupt checkpoint is reached through the same commands
// and is just as permanent.
func TestCorruptCaseExitsTwo(t *testing.T) {
	const caseJSON = `{"id":"c-x","phase":"investigate","work_dir":"","pipeline":"v0"}`
	corruptions := map[string]struct{ caseFile, cpName, cpBody string }{
		"undecodable case.json": {caseFile: `{ not json`},
		"undecodable checkpoint": {
			caseFile: caseJSON, cpName: "01-intake.json", cpBody: `{ not json`,
		},
		"unsupported checkpoint schema": {
			caseFile: caseJSON, cpName: "01-intake.json",
			cpBody: `{"schema_version":99,"phase":"intake","status":"completed","output":null}`,
		},
		"checkpoint with no walk index": {
			caseFile: caseJSON, cpName: "intake.json",
			cpBody: `{"schema_version":1,"phase":"intake","status":"completed","output":null}`,
		},
	}
	for name, c := range corruptions {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			caseID := "c-x"
			caseDir := filepath.Join(work, caseID)
			if err := os.MkdirAll(filepath.Join(caseDir, "checkpoints"), 0o755); err != nil {
				t.Fatalf("creating case dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(caseDir, orchestrator.CaseFile), []byte(c.caseFile), 0o644); err != nil {
				t.Fatalf("writing case file: %v", err)
			}
			if c.cpName != "" {
				path := filepath.Join(caseDir, "checkpoints", c.cpName)
				if err := os.WriteFile(path, []byte(c.cpBody), 0o644); err != nil {
					t.Fatalf("writing checkpoint: %v", err)
				}
			}
			cmds := map[string][]string{
				"resume": {"hunt", "--work", work, "--resume", caseID},
				"cost":   {"cost", "--work", work, caseID},
			}
			for cmd, args := range cmds {
				t.Run(cmd, func(t *testing.T) {
					a, _, _ := stubApp(t)
					err := a.run(t.Context(), args)
					if !errors.Is(err, orchestrator.ErrCorruptCase) {
						t.Fatalf("error = %v, want ErrCorruptCase", err)
					}
					if errors.Is(err, orchestrator.ErrNoSuchCase) {
						t.Error("a corrupt case reported itself as absent; the case is there, it is damaged")
					}
					if got := exitCode(err); got != 2 {
						t.Errorf("exit code = %d, want 2; these bytes will not decode on any retry", got)
					}
				})
			}
		})
	}
}

// TestPhaseFailureStillExitsOneAndLeavesAResumableCase is the other side of the
// #45 reclassification: after moving five refusals to exit 2, exit 1 must still
// have a member, and its documented promise — a failed phase leaves a resumable
// case — must still hold for it. ErrBudgetExhausted is that member: the phase
// was refused for budget, the case is checkpointed as failed, and raising
// --max-tokens and resuming completes it. A change that flattened everything
// into exit 2 would fail here.
func TestPhaseFailureStillExitsOneAndLeavesAResumableCase(t *testing.T) {
	work := t.TempDir()
	caseID := "c-20260728T101500-a1b2c3"
	caseDir := filepath.Join(work, caseID)
	writeCaseFixture(t, caseDir, caseID)

	// Rewind the case to investigate: intake keeps its completed checkpoint, so
	// resume adopts it rather than re-running it, and investigate re-runs
	// against a budget its own failed attempt has already used up.
	c := readCase(t, caseDir)
	c.Phase = orchestrator.PhaseInvestigate
	writeJSONFixture(t, filepath.Join(caseDir, orchestrator.CaseFile), c)
	if err := os.Remove(filepath.Join(caseDir, "checkpoints", "03-report.json")); err != nil {
		t.Fatalf("removing report checkpoint: %v", err)
	}
	writeJSONFixture(t, filepath.Join(caseDir, "checkpoints", "02-investigate.json"), orchestrator.Checkpoint{
		SchemaVersion: orchestrator.CheckpointSchemaVersion,
		CaseID:        caseID,
		Phase:         orchestrator.PhaseInvestigate,
		Status:        orchestrator.StatusFailed,
		Spend:         orchestrator.Spend{InputTokens: 900},
		Output:        json.RawMessage("null"),
	})

	a, _, _ := stubApp(t)
	err := a.run(t.Context(), []string{"hunt", "--work", work, "--resume", caseID, "--max-tokens", "100"})
	if !errors.Is(err, orchestrator.ErrBudgetExhausted) {
		t.Fatalf("error = %v, want ErrBudgetExhausted", err)
	}
	if got := exitCode(err); got != 1 {
		t.Fatalf("exit code = %d, want 1; a failed phase is a fault, and this case is resumable", got)
	}
	// The promise exit 1 makes: there is a case on disk to resume.
	if got := readCase(t, caseDir); got.Phase != orchestrator.PhaseFailed {
		t.Errorf("case phase = %q, want %q — exit 1 promises a resumable case", got.Phase, orchestrator.PhaseFailed)
	}
}

func TestVersionCommand(t *testing.T) {
	a, stdout, _ := stubApp(t)
	if err := a.run(t.Context(), []string{"version"}); err != nil {
		t.Fatalf("version: %v", err)
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Errorf("stdout = %q, want %q", stdout.String(), version)
	}
}

func TestCostReadsCheckpoints(t *testing.T) {
	work := t.TempDir()
	caseID := "c-20260728T101500-a1b2c3"
	caseDir := filepath.Join(work, caseID)
	writeCaseFixture(t, caseDir, caseID)

	a, stdout, _ := stubApp(t)
	if err := a.run(t.Context(), []string{"cost", "--work", work, caseID}); err != nil {
		t.Fatalf("cost: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{caseID, "KubePodCrashLooping", "investigate", "TOTAL", "0.0841"} {
		if !strings.Contains(out, want) {
			t.Errorf("cost output is missing %q:\n%s", want, out)
		}
	}
	// The total is the sum of the checkpoints, not a separately kept number.
	if !strings.Contains(out, "31240") || !strings.Contains(out, "2210") {
		t.Errorf("cost output does not sum the checkpoints:\n%s", out)
	}
}

// writeCaseFixture writes a small finished case work dir by hand, so the cost
// command is tested against the on-disk format rather than through a full run.
func writeCaseFixture(t *testing.T, caseDir, caseID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(caseDir, "checkpoints"), 0o755); err != nil {
		t.Fatalf("creating case dir: %v", err)
	}
	c := orchestrator.Case{
		ID:        caseID,
		AlertName: "KubePodCrashLooping",
		Phase:     orchestrator.PhaseDone,
		WorkDir:   caseDir,
		Pipeline:  orchestrator.PipelineV0,
	}
	writeJSONFixture(t, filepath.Join(caseDir, orchestrator.CaseFile), c)

	start := time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC)
	checkpoints := []orchestrator.Checkpoint{
		{Phase: orchestrator.PhaseIntake, Spend: orchestrator.Spend{WallMS: 12}},
		{Phase: orchestrator.PhaseInvestigate, Spend: orchestrator.Spend{
			InputTokens: 31240, OutputTokens: 2210, USD: 0.0841, ToolCalls: 7, WallMS: 162044,
		}},
		{Phase: orchestrator.PhaseReport, Spend: orchestrator.Spend{WallMS: 3}},
	}
	for i, cp := range checkpoints {
		cp.SchemaVersion = orchestrator.CheckpointSchemaVersion
		cp.CaseID = caseID
		cp.Status = orchestrator.StatusCompleted
		cp.StartedAt = start
		cp.FinishedAt = start.Add(time.Second)
		cp.Output = json.RawMessage("null")
		name := filepath.Join(caseDir, "checkpoints", strings.Join([]string{"0" + string(rune('1'+i)), string(cp.Phase)}, "-")+".json")
		writeJSONFixture(t, name, cp)
	}
}

// writeJSONFixture marshals v into path.
func writeJSONFixture(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshaling fixture: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
}

func TestPricesFor(t *testing.T) {
	in, out, known := pricesFor(DefaultModel)
	if !known {
		t.Fatalf("the default model %q has no published pricing", DefaultModel)
	}
	if in <= 0 || out <= 0 {
		t.Errorf("prices = %v/%v $/MTok, want positive rates", in, out)
	}
	if _, _, known := pricesFor("some-unreleased-model"); known {
		t.Error("an unknown model reported known pricing")
	}
}

// TestProviderSeamIsRaw guards the PR #14 fix: the CLI hands hounds.Compose an
// undecorated provider, and Compose owns the middleware order. A provider that
// arrives pre-wrapped here would put accounting outside retry again.
func TestProviderSeamIsRaw(t *testing.T) {
	a, _, _ := stubApp(t)
	var got provider
	a.newProvider = func(providerConfig) (provider, error) {
		got = rawProvider{}
		return got, nil
	}
	p, err := a.newProvider(providerConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	if _, ok := p.(rawProvider); !ok {
		t.Errorf("provider seam returned %T, want the raw provider", p)
	}
}

// rawProvider is an undecorated llm.Provider used to check the wiring seam.
type rawProvider struct{}

func (rawProvider) Complete(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("not called")
}
func (rawProvider) CountCost(llm.Usage) llm.Cost { return llm.Cost{} }
func (rawProvider) Name() string                 { return "raw" }
