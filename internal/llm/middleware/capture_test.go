package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/llm/llmtest"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenRequest and goldenResponse are the fixed fixtures behind the capture
// format golden test.
func goldenFixtures() (llm.Request, llm.Response) {
	req := llm.Request{
		Model:     "claude-test-1",
		System:    "You are a metrics investigator.",
		MaxTokens: 1024,
		Messages: []llm.Message{
			llm.UserMessage(llm.TextBlock("Investigate the HighErrorRate alert.")),
			llm.AssistantMessage(llm.ToolUseBlock(llm.ToolUse{
				ID:    "toolu_01",
				Name:  "query_range",
				Input: json.RawMessage(`{"query":"up","start":"2026-07-28T10:00:00Z","end":"2026-07-28T11:00:00Z"}`),
			})),
			llm.UserMessage(llm.ToolResultBlock("toolu_01", `{"series":[]}`, false)),
		},
		Tools: []llm.Tool{{
			Name:        "query_range",
			Description: "Run a PromQL range query.",
			InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`),
		}},
		ToolChoice: llm.ToolChoice{Type: llm.ToolChoiceAuto},
	}
	resp := llm.Response{
		Message:    llm.AssistantMessage(llm.TextBlock("The service is down.")),
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 420, OutputTokens: 37},
	}
	return req, resp
}

func TestCaptureFileFormatGolden(t *testing.T) {
	req, resp := goldenFixtures()
	dir := t.TempDir()
	c, err := NewCapture(llmtest.New(t, resp), dir, "metrics-hound")
	if err != nil {
		t.Fatalf("NewCapture: %v", err)
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "llm", "000-metrics-hound.json"))
	if err != nil {
		t.Fatalf("reading capture file: %v", err)
	}

	goldenPath := filepath.Join("testdata", "capture.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("creating testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden (run with -update to regenerate): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("capture file differs from golden.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestCaptureSequencesFiles(t *testing.T) {
	req, resp := goldenFixtures()
	dir := t.TempDir()
	c, err := NewCapture(llmtest.New(t, resp, resp), dir, "hound")
	if err != nil {
		t.Fatalf("NewCapture: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := c.Complete(ctx, req); err != nil {
			t.Fatalf("Complete %d: %v", i, err)
		}
	}
	for _, name := range []string{"000-hound.json", "001-hound.json"} {
		if _, err := os.Stat(filepath.Join(dir, "llm", name)); err != nil {
			t.Errorf("expected capture file %s: %v", name, err)
		}
	}
}

func TestCaptureContinuesSequenceAfterResume(t *testing.T) {
	req, resp := goldenFixtures()
	dir := t.TempDir()
	llmDir := filepath.Join(dir, "llm")
	if err := os.MkdirAll(llmDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Simulate a crashed run that already captured 000..005.
	if err := os.WriteFile(filepath.Join(llmDir, "005-hound.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seeding capture file: %v", err)
	}

	c, err := NewCapture(llmtest.New(t, resp), dir, "hound")
	if err != nil {
		t.Fatalf("NewCapture: %v", err)
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(llmDir, "006-hound.json")); err != nil {
		t.Errorf("expected sequence to continue at 006: %v", err)
	}
}

// TestCaptureContinuesSequencePastThreeDigits pins the resume behaviour once
// a case has run long enough to leave four-digit capture files behind: a
// fixed-width parser skips 1000-hound.json, restarts numbering, and overwrites
// earlier captures.
func TestCaptureContinuesSequencePastThreeDigits(t *testing.T) {
	req, resp := goldenFixtures()
	dir := t.TempDir()
	llmDir := filepath.Join(dir, "llm")
	if err := os.MkdirAll(llmDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"000-hound.json", "999-hound.json", "1000-hound.json"} {
		if err := os.WriteFile(filepath.Join(llmDir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("seeding capture file %s: %v", name, err)
		}
	}

	c, err := NewCapture(llmtest.New(t, resp), dir, "hound")
	if err != nil {
		t.Fatalf("NewCapture: %v", err)
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(llmDir, "1001-hound.json")); err != nil {
		t.Errorf("expected sequence to continue at 1001: %v", err)
	}
	// The seeded files must still be exactly as seeded: a restarted sequence
	// shows up as a capture file overwritten with a real record.
	for _, name := range []string{"000-hound.json", "999-hound.json", "1000-hound.json"} {
		data, err := os.ReadFile(filepath.Join(llmDir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(data) != "{}\n" {
			t.Errorf("%s was overwritten: sequence numbering collided", name)
		}
	}
}

func TestNextSeqParsesSequencePrefix(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  int
	}{
		{"empty dir", nil, 0},
		{"three digits", []string{"005-hound.json"}, 6},
		{"four digits", []string{"1000-hound.json"}, 1001},
		{"mixed widths", []string{"999-hound.json", "1234-hound.json"}, 1235},
		{"label contains a dash", []string{"012-metrics-hound.json"}, 13},
		{"no sequence prefix", []string{"notes.json", "-hound.json", "abc-hound.json"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
					t.Fatalf("seeding %s: %v", name, err)
				}
			}
			got, err := nextSeq(dir)
			if err != nil {
				t.Fatalf("nextSeq: %v", err)
			}
			if got != tt.want {
				t.Errorf("nextSeq = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCaptureRecordsErrors(t *testing.T) {
	req, _ := goldenFixtures()
	dir := t.TempDir()
	c, err := NewCapture(llmtest.New(nil), dir, "hound") // empty queue → error
	if err != nil {
		t.Fatalf("NewCapture: %v", err)
	}
	if _, err := c.Complete(context.Background(), req); !errors.Is(err, llmtest.ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted passed through", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "llm", "000-hound.json"))
	if err != nil {
		t.Fatalf("reading capture file: %v", err)
	}
	var rec CaptureRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshaling capture record: %v", err)
	}
	if rec.Error == "" {
		t.Error("capture record has no error field for a failed attempt")
	}
	if rec.Response != nil {
		t.Error("capture record has a response for a failed attempt")
	}
}
