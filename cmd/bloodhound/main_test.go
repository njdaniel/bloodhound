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
		{name: "cost for an unknown case", args: []string{"cost", "--work", work, "c-nope"}, want: 1},
		{name: "resume an unknown case", args: []string{"hunt", "--work", work, "--resume", "c-nope"}, want: 1},
		{name: "serve is M2", args: []string{"serve"}, want: 1},
		{name: "replay is M2", args: []string{"replay", "c-1"}, want: 1},
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
