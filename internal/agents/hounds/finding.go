package hounds

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// submitPayload is the model-facing shape of submit_finding input: a Finding
// whose evidence cites tool calls by sequence number instead of by capture
// filename.
type submitPayload struct {
	Hound      string                 `json:"hound"`
	Summary    string                 `json:"summary"`
	Confidence float64                `json:"confidence"`
	Onset      string                 `json:"onset,omitempty"`
	Evidence   []submitEvidence       `json:"evidence"`
	DeadEnds   []orchestrator.DeadEnd `json:"dead_ends"`
}

// submitEvidence is one evidence record as the model writes it.
type submitEvidence struct {
	Tool          string `json:"tool"`
	Query         string `json:"query"`
	Observation   string `json:"observation"`
	ToolCallIndex int    `json:"tool_call_index"`
}

// resolveFinding validates raw submit_finding input against the derived
// schema, resolves each cited tool-call index to the capture ref recorded for
// that call, and returns the assembled Finding.
//
// refs holds one capture ref per tool call made so far, in call order; the
// model cites them 1-based. Constraint violations and unresolvable indices
// come back as ValidationErrors for the repair cycle (spec 002 §3.2); a
// non-nil error means the schema assets themselves are broken, which is a bug
// rather than something the model can fix.
func resolveFinding(input json.RawMessage, refs []string) (orchestrator.Finding, ValidationErrors, error) {
	schema, err := submitSchema()
	if err != nil {
		return orchestrator.Finding{}, nil, err
	}
	if errs := schema.Validate(input); len(errs) > 0 {
		return orchestrator.Finding{}, errs, nil
	}

	var p submitPayload
	if err := json.Unmarshal(input, &p); err != nil {
		// Shape was already checked above, so this is a type the schema
		// subset accepted but Go could not decode; report it to the model.
		return orchestrator.Finding{}, ValidationErrors{{Message: fmt.Sprintf("could not decode input: %v", err)}}, nil
	}

	finding := orchestrator.Finding{
		Hound:      p.Hound,
		Summary:    p.Summary,
		Confidence: p.Confidence,
		Evidence:   make([]orchestrator.Evidence, 0, len(p.Evidence)),
		DeadEnds:   make([]orchestrator.DeadEnd, 0, len(p.DeadEnds)),
	}
	finding.DeadEnds = append(finding.DeadEnds, p.DeadEnds...)

	if p.Onset != "" {
		onset, err := time.Parse(time.RFC3339, p.Onset)
		if err != nil {
			// Unreachable while the schema declares format: date-time, but
			// cheap insurance against the asset drifting.
			return orchestrator.Finding{}, ValidationErrors{{Path: "onset", Message: "must be an RFC 3339 date-time"}}, nil
		}
		finding.Onset = &onset
	}

	var errs ValidationErrors
	for i, e := range p.Evidence {
		path := fmt.Sprintf("evidence[%d].%s", i, ToolCallIndexField)
		if e.ToolCallIndex < 1 || e.ToolCallIndex > len(refs) {
			errs = append(errs, ValidationError{Path: path, Message: unresolvableIndexMessage(e.ToolCallIndex, len(refs))})
			continue
		}
		ref := refs[e.ToolCallIndex-1]
		if ref == "" {
			errs = append(errs, ValidationError{Path: path, Message: fmt.Sprintf("tool call #%d produced no capture to cite", e.ToolCallIndex)})
			continue
		}
		finding.Evidence = append(finding.Evidence, orchestrator.Evidence{
			Tool:        e.Tool,
			Query:       e.Query,
			Observation: e.Observation,
			CaptureRef:  ref,
		})
	}
	if len(errs) > 0 {
		return orchestrator.Finding{}, errs, nil
	}

	// Belt and braces: the assembled Finding must itself satisfy the
	// checked-in schema. This catches a Go type that has drifted from the
	// asset — exactly the drift spec 002 §3.3 says must not happen silently.
	assembled, err := json.Marshal(finding)
	if err != nil {
		return orchestrator.Finding{}, nil, fmt.Errorf("marshaling assembled finding: %w", err)
	}
	fschema, err := FindingSchema()
	if err != nil {
		return orchestrator.Finding{}, nil, err
	}
	if errs := fschema.Validate(assembled); len(errs) > 0 {
		return orchestrator.Finding{}, nil, fmt.Errorf("assembled finding violates %s: %w", findingSchemaID, errs)
	}
	return finding, nil, nil
}

// findingSchemaID names the schema asset in error messages.
const findingSchemaID = "bloodhound/finding/v1"

// unresolvableIndexMessage explains an out-of-range tool-call citation in
// terms the model can act on.
func unresolvableIndexMessage(idx, made int) string {
	if made == 0 {
		return fmt.Sprintf("cites tool call #%d, but no tool calls have been made; run a query before citing it as evidence", idx)
	}
	return fmt.Sprintf("cites tool call #%d, which does not exist; cite a number between 1 and %d", idx, made)
}
