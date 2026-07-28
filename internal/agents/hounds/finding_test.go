package hounds

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// submitJSON renders a submit_finding input object with the given evidence
// records spliced in, so each test states only what it is exercising.
func submitJSON(t *testing.T, evidence, extra string) json.RawMessage {
	t.Helper()
	doc := `{
	  "hound": "metrics",
	  "summary": "checkout is OOMKilling under a flat request rate",
	  "confidence": 0.7,
	  "evidence": [` + evidence + `],
	  "dead_ends": [{"theory": "traffic spike", "ruled_out_by": "request rate flat"}]` + extra + `
	}`
	if !json.Valid([]byte(doc)) {
		t.Fatalf("test built invalid JSON: %s", doc)
	}
	return json.RawMessage(doc)
}

// evidenceJSON renders one evidence record citing tool call idx.
func evidenceJSON(idx int, extra string) string {
	return `{"tool":"query_range","query":"up","observation":"observed something",` +
		`"tool_call_index":` + strconv.Itoa(idx) + extra + `}`
}

// TestResolveFindingAssignsCaptureRefsInOrder is the §3.4 contract: the model
// cites tool calls by number and the loop, not the model, supplies the
// capture filename — including when the citations are out of order.
func TestResolveFindingAssignsCaptureRefsInOrder(t *testing.T) {
	refs := []string{"mcp/000-query_range.json", "mcp/001-query_instant.json"}
	input := submitJSON(t, evidenceJSON(2, "")+","+evidenceJSON(1, ""), "")

	finding, verrs, err := resolveFinding(input, refs)
	if err != nil {
		t.Fatalf("resolveFinding: %v", err)
	}
	if len(verrs) > 0 {
		t.Fatalf("validation errors: %v", verrs)
	}
	got := []string{finding.Evidence[0].CaptureRef, finding.Evidence[1].CaptureRef}
	want := []string{refs[1], refs[0]}
	if got[0] != want[0] || got[1] != want[1] {
		t.Errorf("capture refs = %v, want %v", got, want)
	}
	if finding.Hound != MetricsHound {
		t.Errorf("hound = %q, want %q", finding.Hound, MetricsHound)
	}
}

func TestResolveFindingUnresolvableIndex(t *testing.T) {
	tests := []struct {
		name  string
		index int
		refs  []string
		want  string
	}{
		{"past the end", 3, []string{"mcp/000-query_range.json", "mcp/001-query_range.json"},
			"evidence[0].tool_call_index: cites tool call #3, which does not exist; cite a number between 1 and 2"},
		{"no calls made", 1, nil,
			"evidence[0].tool_call_index: cites tool call #1, but no tool calls have been made"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := submitJSON(t, evidenceJSON(tt.index, ""), "")
			_, verrs, err := resolveFinding(input, tt.refs)
			if err != nil {
				t.Fatalf("resolveFinding: %v", err)
			}
			if len(verrs) == 0 {
				t.Fatal("unresolvable index was accepted")
			}
			if !strings.Contains(verrs.Error(), tt.want) {
				t.Errorf("errors = %q, want one containing %q", verrs.Error(), tt.want)
			}
		})
	}
}

// TestResolveFindingRejectsZeroIndex pins the 1-based citation numbering: a
// model that counts from zero is corrected rather than silently off by one.
func TestResolveFindingRejectsZeroIndex(t *testing.T) {
	input := submitJSON(t, evidenceJSON(0, ""), "")
	_, verrs, err := resolveFinding(input, []string{"mcp/000-query_range.json"})
	if err != nil {
		t.Fatalf("resolveFinding: %v", err)
	}
	if len(verrs) == 0 {
		t.Fatal("index 0 was accepted, want a validation error")
	}
}

// TestResolveFindingRejectsModelAuthoredCaptureRef is the other half of §3.4:
// the model must not be able to write a capture filename at all.
func TestResolveFindingRejectsModelAuthoredCaptureRef(t *testing.T) {
	input := submitJSON(t, evidenceJSON(1, `,"capture_ref":"mcp/999-invented.json"`), "")
	_, verrs, err := resolveFinding(input, []string{"mcp/000-query_range.json"})
	if err != nil {
		t.Fatalf("resolveFinding: %v", err)
	}
	if len(verrs) == 0 {
		t.Fatal("model-authored capture_ref was accepted")
	}
	if !strings.Contains(verrs.Error(), "capture_ref: is not an allowed property") {
		t.Errorf("errors = %q, want the capture_ref property rejected", verrs.Error())
	}
}

func TestResolveFindingSchemaViolation(t *testing.T) {
	input := json.RawMessage(`{"hound":"metrics","summary":"x","confidence":4,"evidence":[],"dead_ends":[]}`)
	_, verrs, err := resolveFinding(input, nil)
	if err != nil {
		t.Fatalf("resolveFinding: %v", err)
	}
	if !strings.Contains(verrs.Error(), "confidence: must be <= 1") {
		t.Errorf("errors = %q, want the confidence range violation", verrs.Error())
	}
}

func TestResolveFindingParsesOnset(t *testing.T) {
	input := submitJSON(t, "", `, "onset": "2026-07-28T10:12:00Z"`)
	finding, verrs, err := resolveFinding(input, nil)
	if err != nil {
		t.Fatalf("resolveFinding: %v", err)
	}
	if len(verrs) > 0 {
		t.Fatalf("validation errors: %v", verrs)
	}
	if finding.Onset == nil || !finding.Onset.Equal(time.Date(2026, 7, 28, 10, 12, 0, 0, time.UTC)) {
		t.Errorf("onset = %v, want 2026-07-28T10:12:00Z", finding.Onset)
	}
}

// TestResolveFindingEmitsSchemaValidFinding closes the loop between the Go
// type and the checked-in asset: whatever resolveFinding returns must
// validate against finding.v1.json.
func TestResolveFindingEmitsSchemaValidFinding(t *testing.T) {
	input := submitJSON(t, evidenceJSON(1, ""), `, "onset": "2026-07-28T10:12:00Z"`)
	finding, verrs, err := resolveFinding(input, []string{"mcp/000-query_range.json"})
	if err != nil {
		t.Fatalf("resolveFinding: %v", err)
	}
	if len(verrs) > 0 {
		t.Fatalf("validation errors: %v", verrs)
	}
	encoded, err := json.Marshal(finding)
	if err != nil {
		t.Fatalf("marshaling finding: %v", err)
	}
	schema, err := FindingSchema()
	if err != nil {
		t.Fatalf("FindingSchema: %v", err)
	}
	if errs := schema.Validate(encoded); len(errs) > 0 {
		t.Fatalf("resolved finding violates the checked-in schema: %v\n%s", errs, encoded)
	}
}
