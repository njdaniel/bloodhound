package hounds

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFixture loads a Finding fixture from testdata/finding.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "finding", name+".json"))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func TestFindingSchemaParses(t *testing.T) {
	s, err := FindingSchema()
	if err != nil {
		t.Fatalf("FindingSchema: %v", err)
	}
	if s.ID != findingSchemaID {
		t.Errorf("schema $id = %q, want %q", s.ID, findingSchemaID)
	}
	// Every keyword in the checked-in asset must be one the validator
	// enforces; ParseSchema is what proves it, and this pins the intent.
	if _, ok := s.Properties["evidence"]; !ok {
		t.Error("schema has no evidence property")
	}
}

// TestFindingSchemaPinsHoundName keeps the Go constant and the schema's const
// from drifting apart.
func TestFindingSchemaPinsHoundName(t *testing.T) {
	s, err := FindingSchema()
	if err != nil {
		t.Fatalf("FindingSchema: %v", err)
	}
	hound, ok := s.Properties["hound"]
	if !ok || hound.Const == nil {
		t.Fatal("schema does not pin the hound name with a const")
	}
	var name string
	if err := json.Unmarshal(*hound.Const, &name); err != nil {
		t.Fatalf("decoding hound const: %v", err)
	}
	if name != MetricsHound {
		t.Errorf("schema hound const = %q, MetricsHound = %q", name, MetricsHound)
	}
}

func TestFindingSchemaAcceptsValidFixture(t *testing.T) {
	s, err := FindingSchema()
	if err != nil {
		t.Fatalf("FindingSchema: %v", err)
	}
	if errs := s.Validate(readFixture(t, "valid")); len(errs) > 0 {
		t.Fatalf("valid fixture rejected: %v", errs)
	}
}

// TestFindingSchemaRejectsConstraintViolations walks one fixture per
// constraint in the schema (spec 002 §5) and asserts both that it is rejected
// and that the message names the offending path.
func TestFindingSchemaRejectsConstraintViolations(t *testing.T) {
	tests := []struct {
		fixture string
		want    string // substring of the expected first error
	}{
		{"missing-summary", "summary: is required"},
		{"missing-dead-ends", "dead_ends: is required"},
		{"unknown-property", "cluster: is not an allowed property"},
		{"wrong-hound", `hound: must be "metrics"`},
		{"confidence-above-max", "confidence: must be <= 1"},
		{"confidence-below-min", "confidence: must be >= 0"},
		{"confidence-wrong-type", "confidence: must be a number, got string"},
		{"summary-empty", "summary: must be at least 1 characters"},
		{"summary-too-long", "summary: must be at most 1200 characters, got 1201"},
		{"onset-not-date-time", "onset: must be an RFC 3339 date-time"},
		{"evidence-not-array", "evidence: must be an array, got string"},
		{"evidence-too-many", "evidence: must have at most 12 items, got 13"},
		{"evidence-missing-capture-ref", "evidence[0].capture_ref: is required"},
		{"evidence-unknown-property", "evidence[0].screenshot: is not an allowed property"},
		{"evidence-query-too-long", "evidence[0].query: must be at most 500 characters"},
		{"evidence-observation-too-long", "evidence[1].observation: must be at most 500 characters"},
		{"dead-ends-too-many", "dead_ends: must have at most 8 items, got 9"},
		{"dead-end-missing-ruled-out-by", "dead_ends[0].ruled_out_by: is required"},
		{"dead-end-theory-too-long", "dead_ends[0].theory: must be at most 300 characters"},
	}
	s, err := FindingSchema()
	if err != nil {
		t.Fatalf("FindingSchema: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			errs := s.Validate(readFixture(t, tt.fixture))
			if len(errs) == 0 {
				t.Fatalf("fixture %s was accepted, want a violation of %q", tt.fixture, tt.want)
			}
			if !strings.Contains(errs.Error(), tt.want) {
				t.Errorf("errors = %q, want one containing %q", errs.Error(), tt.want)
			}
		})
	}
}

// TestSubmitSchemaSwapsCaptureRefForIndex pins the §3.4 contract at the
// schema level: the model is never shown a capture_ref field to fill in.
func TestSubmitSchemaSwapsCaptureRefForIndex(t *testing.T) {
	doc, err := SubmitFindingSchemaJSON()
	if err != nil {
		t.Fatalf("SubmitFindingSchemaJSON: %v", err)
	}
	var root struct {
		Properties struct {
			Evidence struct {
				MaxItems int `json:"maxItems"`
				Items    struct {
					Required   []string                   `json:"required"`
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"evidence"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(doc, &root); err != nil {
		t.Fatalf("decoding submit schema: %v", err)
	}
	items := root.Properties.Evidence.Items
	if _, ok := items.Properties[captureRefField]; ok {
		t.Errorf("submit schema still exposes %s to the model", captureRefField)
	}
	if _, ok := items.Properties[ToolCallIndexField]; !ok {
		t.Errorf("submit schema is missing %s", ToolCallIndexField)
	}
	var required []string
	required = append(required, items.Required...)
	if !contains(required, ToolCallIndexField) || contains(required, captureRefField) {
		t.Errorf("evidence required = %v, want %s and not %s", required, ToolCallIndexField, captureRefField)
	}
	// Constraints carried over from the Finding schema must survive the
	// rewrite: the derived schema is the same contract, minus one field.
	if root.Properties.Evidence.MaxItems != 12 {
		t.Errorf("evidence maxItems = %d, want 12 carried through from the Finding schema", root.Properties.Evidence.MaxItems)
	}
	// The derived document must itself be parseable by the validator.
	if _, err := submitSchema(); err != nil {
		t.Fatalf("submitSchema: %v", err)
	}
}

// TestParseSchemaRejectsUnsupportedKeyword guards the failure mode that would
// let the checked-in asset grow a constraint the validator silently ignores.
func TestParseSchemaRejectsUnsupportedKeyword(t *testing.T) {
	doc := []byte(`{"type":"object","properties":{"a":{"type":"string","pattern":"^x"}}}`)
	if _, err := ParseSchema(doc); err == nil {
		t.Fatal("ParseSchema accepted an unsupported keyword")
	} else if !strings.Contains(err.Error(), "pattern") {
		t.Errorf("error = %v, want it to name the unsupported keyword", err)
	}
}

func TestValidateReportsInvalidJSON(t *testing.T) {
	s, err := FindingSchema()
	if err != nil {
		t.Fatalf("FindingSchema: %v", err)
	}
	errs := s.Validate([]byte(`{"hound":`))
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "not valid JSON") {
		t.Fatalf("errs = %v, want a not-valid-JSON error", errs)
	}
}

// TestMetricsSystemPromptAsset checks the prompt is a real checked-in asset
// carrying the instructions spec 002 §3.2 requires.
func TestMetricsSystemPromptAsset(t *testing.T) {
	p := MetricsSystemPrompt()
	for _, want := range []string{"onset", "rate(", "Narrow selectors", "Dead ends", "submit_finding"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt is missing %q", want)
		}
	}
}

// contains reports whether s contains v.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
