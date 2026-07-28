package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/llm/llmtest"
	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// The end-to-end tests below drive the real CLI in-process against the real
// mcp-prom binary, with only two things faked: the model (a scripted
// llmtest.Provider) and Prometheus (an httptest server serving canned
// /api/v1 JSON). Nothing reaches the network and nothing calls a paid API
// (spec 002 §5).

// alertFixture is the Alertmanager payload the e2e cases investigate.
const alertFixture = `{
  "version": "4",
  "status": "firing",
  "alerts": [
    {
      "status": "firing",
      "labels": {"alertname": "KubePodCrashLooping", "namespace": "shop", "pod": "checkout-7d9f", "severity": "critical"},
      "annotations": {"summary": "checkout is restarting"},
      "startsAt": "2026-07-28T10:05:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "fingerprint": "a1b2c3d4"
    }
  ]
}`

// promQueryRangeBody is the canned matrix Prometheus returns for the one
// query_range call the scripted model makes.
const promQueryRangeBody = `{"status":"success","data":{"resultType":"matrix","result":[
  {"metric":{"namespace":"shop","pod":"checkout-7d9f"},
   "values":[[1785232800,"0.1"],[1785232830,"420000000"],[1785232860,"530000000"]]}
]}}`

// mcpPromBinary builds the mcp-prom server once per test binary and returns
// its path. The hound talks to the real server over stdio, so the e2e test
// exercises the actual MCP wiring rather than a stand-in.
var mcpPromBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "bloodhound-e2e-*")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mcp-prom")
	cmd := exec.Command("go", "build", "-o", path, "github.com/njdaniel/bloodhound/mcp/prom")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("building mcp-prom: %w: %s", err, out)
	}
	return path, nil
})

// buildMCPProm returns the path to the built mcp-prom binary.
func buildMCPProm(t *testing.T) string {
	t.Helper()
	path, err := mcpPromBinary()
	if err != nil {
		t.Fatalf("%v", err)
	}
	return path
}

// startFakeProm starts an httptest Prometheus serving canned responses.
func startFakeProm(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/query_range":
			fmt.Fprint(w, promQueryRangeBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// queryRangeCall is the tool_use block the scripted model emits.
func queryRangeCall(id string) llm.Response {
	input := json.RawMessage(`{
      "query": "container_memory_working_set_bytes{namespace=\"shop\",pod=~\"checkout-.*\"}",
      "start": "2026-07-28T10:00:00Z",
      "end": "2026-07-28T10:10:00Z"
    }`)
	return llm.Response{
		Message: llm.AssistantMessage(llm.ToolUseBlock(llm.ToolUse{
			ID: id, Name: "query_range", Input: input,
		})),
		StopReason: llm.StopToolUse,
		Usage:      llm.Usage{InputTokens: 1200, OutputTokens: 80},
	}
}

// submitFindingCall is the terminating tool_use block: a schema-valid finding
// citing tool call #1.
func submitFindingCall(id string) llm.Response {
	input := json.RawMessage(`{
      "hound": "metrics",
      "summary": "checkout's working set climbs to its memory limit inside the alert window; the restarts are OOMKills, not a traffic spike.",
      "confidence": 0.82,
      "onset": "2026-07-28T10:02:00Z",
      "evidence": [
        {
          "tool": "query_range",
          "query": "container_memory_working_set_bytes{namespace=\"shop\",pod=~\"checkout-.*\"}",
          "observation": "working set rises to 530MB against a 512Mi limit inside the window",
          "tool_call_index": 1
        }
      ],
      "dead_ends": [
        {
          "theory": "a traffic spike drove the restarts",
          "ruled_out_by": "request rate was not queried; nothing in the metrics window supports it"
        }
      ]
    }`)
	return llm.Response{
		Message: llm.AssistantMessage(llm.ToolUseBlock(llm.ToolUse{
			ID: id, Name: "submit_finding", Input: input,
		})),
		StopReason: llm.StopToolUse,
		Usage:      llm.Usage{InputTokens: 2400, OutputTokens: 320},
	}
}

// newTestApp builds an app wired to a scripted provider and the real MCP
// session, writing its output into buffers the test can read.
func newTestApp(t *testing.T, p llm.Provider, env map[string]string) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, func(key string) string { return env[key] })
	a.now = func() time.Time { return time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC) }
	a.newProvider = func(providerConfig) (provider, error) { return p, nil }
	return a, &stdout, &stderr
}

// caseDirs lists the case work dirs under a work root.
func caseDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading work root: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs
}

// captureNames lists the capture files of one stream ("llm" or "mcp").
func captureNames(t *testing.T, caseDir, stream string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(caseDir, orchestrator.CaptureDir, stream))
	if err != nil {
		t.Fatalf("reading %s captures: %v", stream, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestHuntEndToEnd(t *testing.T) {
	work := t.TempDir()
	alertDir := t.TempDir()
	alert := filepath.Join(alertDir, "alert.json")
	if err := os.WriteFile(alert, []byte(alertFixture), 0o644); err != nil {
		t.Fatalf("writing alert: %v", err)
	}

	p := llmtest.New(t, queryRangeCall("t1"), submitFindingCall("t2"))
	p.InputUSDPerMTok = 5
	p.OutputUSDPerMTok = 25
	a, stdout, stderr := newTestApp(t, p, nil)

	err := a.run(t.Context(), []string{
		"hunt",
		"--alert", alert,
		"--work", work,
		"--model", "test-model",
		"--mcp-prom", buildMCPProm(t),
		"--prom-url", startFakeProm(t),
	})
	if err != nil {
		t.Fatalf("hunt: %v\nstderr: %s", err, stderr)
	}
	if code := exitCode(err); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	dirs := caseDirs(t, work)
	if len(dirs) != 1 {
		t.Fatalf("case dirs = %v, want exactly one", dirs)
	}
	caseDir := dirs[0]

	// The work dir is complete: case, checkpoints, both capture streams, and
	// both report artifacts (spec 002 §4.2).
	for _, rel := range []string{
		orchestrator.CaseFile,
		orchestrator.ReportJSON,
		orchestrator.ReportText,
		filepath.Join("checkpoints", "01-intake.json"),
		filepath.Join("checkpoints", "02-investigate.json"),
		filepath.Join("checkpoints", "03-report.json"),
		filepath.Join(orchestrator.CaptureDir, "llm", "000-metrics-hound.json"),
		filepath.Join(orchestrator.CaptureDir, "mcp", "000-query_range.json"),
	} {
		if _, err := os.Stat(filepath.Join(caseDir, rel)); err != nil {
			t.Errorf("work dir is missing %s: %v", rel, err)
		}
	}

	// The rendered report reached stdout.
	if !strings.Contains(stdout.String(), "HYPOTHESIS") || !strings.Contains(stdout.String(), "work dir:") {
		t.Errorf("stdout does not look like a report:\n%s", stdout)
	}

	// report.json matches the golden, with the case ID and timings — the only
	// values that legitimately differ per run — normalized away.
	got, err := os.ReadFile(filepath.Join(caseDir, orchestrator.ReportJSON))
	if err != nil {
		t.Fatalf("reading report.json: %v", err)
	}
	checkGolden(t, "report.golden.json", normalizeReport(t, got))

	// Spend is real: the accounting middleware saw both scripted calls.
	c, checkpoints, total, err := orchestrator.Cost(work, filepath.Base(caseDir))
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if c.Phase != orchestrator.PhaseDone {
		t.Errorf("case phase = %q, want %q", c.Phase, orchestrator.PhaseDone)
	}
	if len(checkpoints) != 3 {
		t.Fatalf("checkpoints = %d, want 3", len(checkpoints))
	}
	if want := 1200 + 80 + 2400 + 320; total.Tokens() != want {
		t.Errorf("total tokens = %d, want %d (both model calls)", total.Tokens(), want)
	}
	if total.ToolCalls != 1 {
		t.Errorf("total tool calls = %d, want 1", total.ToolCalls)
	}
	if total.USD <= 0 {
		t.Errorf("total USD = %v, want the scripted provider's pricing applied", total.USD)
	}
}

// TestHuntResumeContinuesCaptureNumbering is the spec 002 §4.3 acceptance
// test: a run that dies mid-investigate is resumed, the completed phases are
// loaded rather than re-run, and the capture sequence numbers continue instead
// of restarting at 000.
func TestHuntResumeContinuesCaptureNumbering(t *testing.T) {
	work := t.TempDir()
	alert := filepath.Join(t.TempDir(), "alert.json")
	if err := os.WriteFile(alert, []byte(alertFixture), 0o644); err != nil {
		t.Fatalf("writing alert: %v", err)
	}
	mcpProm := buildMCPProm(t)
	promURL := startFakeProm(t)

	// First run: the model makes one tool call, then the provider dies — the
	// in-process stand-in for kill -9 mid-investigate.
	dying := llmtest.New(nil, queryRangeCall("t1"))
	a, _, _ := newTestApp(t, dying, nil)
	args := []string{"hunt", "--alert", alert, "--work", work, "--model", "test-model", "--mcp-prom", mcpProm, "--prom-url", promURL}
	err := a.run(t.Context(), args)
	if err == nil {
		t.Fatal("first hunt succeeded, want the provider failure")
	}
	if code := exitCode(err); code != 1 {
		t.Errorf("exit code = %d, want 1 for a phase failure", code)
	}

	dirs := caseDirs(t, work)
	if len(dirs) != 1 {
		t.Fatalf("case dirs = %v, want exactly one", dirs)
	}
	caseDir := dirs[0]
	caseID := filepath.Base(caseDir)

	llmBefore := captureNames(t, caseDir, "llm")
	mcpBefore := captureNames(t, caseDir, "mcp")
	if len(llmBefore) != 2 || llmBefore[0] != "000-metrics-hound.json" {
		t.Fatalf("llm captures after the first run = %v, want 000 and 001", llmBefore)
	}
	if len(mcpBefore) != 1 || mcpBefore[0] != "000-query_range.json" {
		t.Fatalf("mcp captures after the first run = %v, want 000", mcpBefore)
	}
	before := readCase(t, caseDir)
	if before.Phase != orchestrator.PhaseFailed {
		t.Errorf("case phase = %q, want %q", before.Phase, orchestrator.PhaseFailed)
	}
	intakeBefore := readCheckpoint(t, caseDir, "01-intake.json")

	// Resume: a fresh scripted provider finishes the investigation.
	resumed := llmtest.New(t, queryRangeCall("t3"), submitFindingCall("t4"))
	b, stdout, stderr := newTestApp(t, resumed, nil)
	if err := b.run(t.Context(), []string{
		"hunt", "--resume", caseID, "--work", work,
		"--model", "test-model", "--mcp-prom", mcpProm, "--prom-url", promURL,
	}); err != nil {
		t.Fatalf("resume: %v\nstderr: %s", err, stderr)
	}

	// Intake was loaded, not re-executed.
	intakeAfter := readCheckpoint(t, caseDir, "01-intake.json")
	if !intakeAfter.StartedAt.Equal(intakeBefore.StartedAt) {
		t.Errorf("intake checkpoint was rewritten (%s → %s); it must be loaded from disk",
			intakeBefore.StartedAt, intakeAfter.StartedAt)
	}

	// Capture numbering continued rather than resetting to 000.
	llmAfter := captureNames(t, caseDir, "llm")
	mcpAfter := captureNames(t, caseDir, "mcp")
	if len(llmAfter) != len(llmBefore)+2 {
		t.Errorf("llm captures = %v, want two more than %v", llmAfter, llmBefore)
	}
	if got := llmAfter[len(llmAfter)-1]; got != "003-metrics-hound.json" {
		t.Errorf("last llm capture = %q, want 003-metrics-hound.json (numbering continued)", got)
	}
	if len(mcpAfter) != 2 || mcpAfter[1] != "001-query_range.json" {
		t.Errorf("mcp captures = %v, want 000 and a continued 001", mcpAfter)
	}

	if got := readCase(t, caseDir); got.Phase != orchestrator.PhaseDone {
		t.Errorf("case phase after resume = %q, want %q", got.Phase, orchestrator.PhaseDone)
	}
	if !strings.Contains(stdout.String(), "HYPOTHESIS") {
		t.Errorf("resumed run printed no report:\n%s", stdout)
	}

	// The failed attempt's spend survives the checkpoint overwrite: a crash
	// loop must not be able to hide what it cost.
	_, _, total, err := orchestrator.Cost(work, caseID)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if want := (1200 + 80) + (1200 + 80 + 2400 + 320); total.Tokens() != want {
		t.Errorf("total tokens = %d, want %d (both attempts)", total.Tokens(), want)
	}
}

// readCase loads a case file or fails the test.
func readCase(t *testing.T, caseDir string) orchestrator.Case {
	t.Helper()
	c, err := orchestrator.ReadCase(caseDir)
	if err != nil {
		t.Fatalf("reading case: %v", err)
	}
	return c
}

// readCheckpoint loads one checkpoint file by name.
func readCheckpoint(t *testing.T, caseDir, name string) orchestrator.Checkpoint {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(caseDir, "checkpoints", name))
	if err != nil {
		t.Fatalf("reading checkpoint %s: %v", name, err)
	}
	var cp orchestrator.Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatalf("decoding checkpoint %s: %v", name, err)
	}
	return cp
}

// normalizeReport blanks the fields of report.json that legitimately differ
// between runs — the random case ID, the render timestamp, and wall clock —
// so the rest can be compared byte for byte against a golden.
func normalizeReport(t *testing.T, data []byte) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding report.json: %v", err)
	}
	id, _ := doc["case_id"].(string)
	if !strings.HasPrefix(id, "c-") || len(id) != len("c-20260728T101500-a1b2c3") {
		t.Errorf("case_id = %q, want the c-<timestamp>-<6 hex> form", id)
	}
	doc["case_id"] = "c-NORMALIZED"
	doc["generated_at"] = "NORMALIZED"
	if spend, ok := doc["spend"].(map[string]any); ok {
		spend["wall_ms"] = 0
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("re-encoding report.json: %v", err)
	}
	return append(out, '\n')
}
