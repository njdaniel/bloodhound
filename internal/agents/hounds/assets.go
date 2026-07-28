package hounds

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// findingSchemaV1 is the Finding v1 JSON Schema (spec 002 §3.3), checked in as
// a versioned asset so the contract is reviewable as data rather than as a
// string literal assembled at runtime.
//
//go:embed schema/finding.v1.json
var findingSchemaV1 []byte

// metricsPromptV1 is metrics-hound's system prompt, checked in as a versioned
// asset for the same reason: prompt changes are diffable and reviewable.
//
//go:embed prompt/metrics.v1.md
var metricsPromptV1 string

// MetricsHound is the hound name recorded in Findings this package produces.
// It must equal the value the Finding schema pins with
// "hound": {"const": "metrics"}, which TestFindingSchemaPinsHoundName checks.
const MetricsHound = "metrics"

// ToolCallIndexField is the evidence field the model fills in to cite a tool
// call by its sequence number. The loop replaces it with the corresponding
// capture_ref (spec 002 §3.4); the model never authors a capture_ref.
const ToolCallIndexField = "tool_call_index"

// captureRefField is the evidence field the loop injects, and the one the
// submit_finding schema removes before the schema is shown to the model.
const captureRefField = "capture_ref"

// FindingSchemaJSON returns the checked-in Finding v1 JSON Schema document.
// The returned slice is a copy; callers may modify it freely.
func FindingSchemaJSON() []byte {
	return append([]byte(nil), findingSchemaV1...)
}

// MetricsSystemPrompt returns metrics-hound's checked-in system prompt.
func MetricsSystemPrompt() string { return metricsPromptV1 }

// findingSchema parses the Finding schema once. Parsing is deferred rather
// than done in init so an asset problem surfaces as a wrapped error from Run
// (and from the schema tests), not as a panic at process start.
var findingSchema = sync.OnceValues(func() (*Schema, error) {
	s, err := ParseSchema(findingSchemaV1)
	if err != nil {
		return nil, fmt.Errorf("parsing finding schema asset: %w", err)
	}
	return s, nil
})

// FindingSchema returns the parsed Finding v1 schema, which validates a
// Finding after the loop has injected capture refs.
func FindingSchema() (*Schema, error) { return findingSchema() }

// submitSchemaDoc derives the submit_finding input schema from the Finding
// schema once.
var submitSchemaDoc = sync.OnceValues(func() ([]byte, error) {
	doc, err := deriveSubmitSchema(findingSchemaV1)
	if err != nil {
		return nil, fmt.Errorf("deriving submit_finding schema: %w", err)
	}
	return doc, nil
})

// submitSchema parses the derived submit_finding schema once.
var submitSchema = sync.OnceValues(func() (*Schema, error) {
	doc, err := submitSchemaDoc()
	if err != nil {
		return nil, err
	}
	s, err := ParseSchema(doc)
	if err != nil {
		return nil, fmt.Errorf("parsing submit_finding schema: %w", err)
	}
	return s, nil
})

// SubmitFindingSchemaJSON returns the input schema of the synthetic
// submit_finding tool: the Finding schema with the loop-owned capture_ref
// field swapped for the tool-call index the model cites (spec 002 §3.4).
// Deriving it keeps one checked-in source of truth for the two shapes.
func SubmitFindingSchemaJSON() ([]byte, error) {
	doc, err := submitSchemaDoc()
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), doc...), nil
}

// deriveSubmitSchema rewrites the Finding schema into the model-facing
// submit_finding input schema: evidence items require a tool_call_index
// integer instead of a capture_ref string. Everything else — the caps, the
// confidence range, additionalProperties:false — is carried through
// unchanged, so a constraint added to the checked-in asset reaches the model
// automatically.
func deriveSubmitSchema(doc []byte) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("decoding finding schema: %w", err)
	}
	root["$id"] = "bloodhound/submit_finding/v1"

	items, err := evidenceItems(root)
	if err != nil {
		return nil, err
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("finding schema: evidence items have no properties object")
	}
	if _, ok := props[captureRefField]; !ok {
		return nil, fmt.Errorf("finding schema: evidence items have no %s property", captureRefField)
	}
	delete(props, captureRefField)
	props[ToolCallIndexField] = map[string]any{
		"type":    "integer",
		"minimum": 1,
		"description": "The number N from the [tool call #N] marker on the tool " +
			"result this observation came from. The loop resolves it to the " +
			"stored capture file; do not write a filename.",
	}

	required, ok := items["required"].([]any)
	if !ok {
		return nil, fmt.Errorf("finding schema: evidence items have no required list")
	}
	swapped := make([]any, 0, len(required))
	for _, r := range required {
		if r == captureRefField {
			swapped = append(swapped, ToolCallIndexField)
			continue
		}
		swapped = append(swapped, r)
	}
	items["required"] = swapped

	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encoding submit_finding schema: %w", err)
	}
	return out, nil
}

// evidenceItems navigates to properties.evidence.items in a decoded schema.
func evidenceItems(root map[string]any) (map[string]any, error) {
	props, ok := root["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("finding schema: no properties object")
	}
	evidence, ok := props["evidence"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("finding schema: no evidence property")
	}
	items, ok := evidence["items"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("finding schema: evidence has no items schema")
	}
	return items, nil
}
