package reporter

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// update rewrites the golden files instead of comparing against them:
// go test ./internal/agents/reporter -update
var update = flag.Bool("update", false, "rewrite golden report files")

// testInput is a fully populated report input: every section of the report has
// something in it, so the golden covers the whole layout.
func testInput() orchestrator.ReportInput {
	onset := time.Date(2026, 7, 28, 10, 2, 0, 0, time.UTC)
	return orchestrator.ReportInput{
		Case: orchestrator.Case{
			ID:        "c-20260728T101500-a1b2c3",
			AlertName: "KubePodCrashLooping",
			Labels: map[string]string{
				"alertname": "KubePodCrashLooping",
				"namespace": "shop",
				"pod":       "checkout-7d9f4c8b6-x2k9p",
				"severity":  "critical",
			},
			FiringSince: time.Date(2026, 7, 28, 10, 5, 0, 0, time.UTC),
			WorkDir:     "work/c-20260728T101500-a1b2c3",
		},
		Finding: orchestrator.Finding{
			Hound: "metrics",
			Summary: "checkout is being OOMKilled: its working set climbs to the 512Mi limit " +
				"about ninety seconds after each start, and the restart count steps up in lockstep. " +
				"The climb begins at 10:02, three minutes before the alert fired.",
			Confidence: 0.82,
			Onset:      &onset,
			Evidence: []orchestrator.Evidence{
				{
					Tool:        "query_range",
					Query:       `container_memory_working_set_bytes{namespace="shop",pod=~"checkout-.*"}`,
					Observation: "working set rises from 180Mi to the 512Mi limit over 90s, then resets",
					CaptureRef:  "mcp/000-query_range.json",
				},
				{
					Tool:        "query_instant",
					Query:       `kube_pod_container_status_restarts_total{namespace="shop"}`,
					Observation: "checkout restarts 14 times in the window; no other pod restarts",
					CaptureRef:  "mcp/001-query_instant.json",
				},
			},
			DeadEnds: []orchestrator.DeadEnd{
				{
					Theory:     "an upstream dependency started returning errors",
					RuledOutBy: "upstream 5xx rate is flat at zero across the whole window",
				},
				{
					Theory:     "node-level CPU starvation",
					RuledOutBy: "node CPU saturation stays below 40% and no other pod on the node is affected",
				},
			},
		},
		Spend:       orchestrator.Spend{InputTokens: 31240, OutputTokens: 2210, USD: 0.0841, ToolCalls: 7, WallMS: 162044},
		GeneratedAt: time.Date(2026, 7, 28, 10, 17, 42, 0, time.UTC),
	}
}

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

func TestRenderGolden(t *testing.T) {
	artifacts, err := Render(t.Context(), testInput())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifacts = %d, want report.json and report.txt", len(artifacts))
	}
	if artifacts[0].Name != orchestrator.ReportJSON || artifacts[1].Name != orchestrator.ReportText {
		t.Fatalf("artifact names = %q, %q", artifacts[0].Name, artifacts[1].Name)
	}
	checkGolden(t, "report.golden.json", artifacts[0].Data)
	checkGolden(t, "report.golden.txt", artifacts[1].Data)
}

func TestRenderIsDeterministic(t *testing.T) {
	first, err := Render(t.Context(), testInput())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	second, err := Render(t.Context(), testInput())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for i := range first {
		if string(first[i].Data) != string(second[i].Data) {
			t.Errorf("%s differs between renders of the same input", first[i].Name)
		}
	}
}

func TestReportJSONSchema(t *testing.T) {
	artifacts, err := Render(t.Context(), testInput())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var got Report
	if err := json.Unmarshal(artifacts[0].Data, &got); err != nil {
		t.Fatalf("report.json does not round-trip: %v", err)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if got.CaseID != "c-20260728T101500-a1b2c3" {
		t.Errorf("case_id = %q", got.CaseID)
	}
	if got.Confidence != 0.82 {
		t.Errorf("confidence = %v, want 0.82", got.Confidence)
	}
	if len(got.Evidence) != 2 || got.Evidence[0].CaptureRef != "mcp/000-query_range.json" {
		t.Errorf("evidence = %+v, want the capture refs preserved", got.Evidence)
	}
	if len(got.DeadEnds) != 2 {
		t.Errorf("dead_ends = %d, want 2 — dead ends are first-class output", len(got.DeadEnds))
	}
	if got.Spend != testInput().Spend {
		t.Errorf("spend = %+v, want the case total", got.Spend)
	}
}

func TestTextHandlesAnEmptyFinding(t *testing.T) {
	got := Text(New(orchestrator.ReportInput{
		Case:        orchestrator.Case{ID: "c-1"},
		GeneratedAt: time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC),
	}))
	for _, want := range []string{"c-1", "(no summary)", "EVIDENCE", "(none recorded)", "DEAD ENDS", "spend:"} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
}

func TestTextRowsStayOnOneLine(t *testing.T) {
	in := testInput()
	in.Finding.Evidence = append(in.Finding.Evidence, orchestrator.Evidence{
		Tool:        "query_range",
		Query:       strings.Repeat("very_long_metric_name_", 20),
		Observation: strings.Repeat("wordy ", 40),
		CaptureRef:  "mcp/002-query_range.json",
	})
	for _, line := range strings.Split(Text(New(in)), "\n") {
		if strings.Contains(line, "mcp/002-query_range.json") && len([]rune(line)) > 120 {
			t.Errorf("evidence row is %d runes long; long cells must be elided:\n%s", len([]rune(line)), line)
		}
	}
}

func TestElide(t *testing.T) {
	tests := []struct {
		in    string
		width int
		want  string
	}{
		{in: "short", width: 10, want: "short"},
		{in: "exactly-10", width: 10, want: "exactly-10"},
		{in: "far too long for this", width: 10, want: "far too l…"},
		{in: "collapses\n  whitespace", width: 30, want: "collapses whitespace"},
	}
	for _, tt := range tests {
		if got := elide(tt.in, tt.width); got != tt.want {
			t.Errorf("elide(%q, %d) = %q, want %q", tt.in, tt.width, got, tt.want)
		}
	}
}

func TestWrapKeepsLinesUnderWidth(t *testing.T) {
	got := wrap(strings.Repeat("word ", 60), 40)
	for _, line := range strings.Split(got, "\n") {
		if len([]rune(line)) > 40 {
			t.Errorf("wrapped line is %d runes: %q", len([]rune(line)), line)
		}
	}
	long := strings.Repeat("x", 80)
	if wrap(long, 40) != long {
		t.Error("wrap split a single long token; it must be left intact")
	}
}
