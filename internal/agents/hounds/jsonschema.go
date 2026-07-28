package hounds

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Schema is the subset of JSON Schema bloodhound validates model output
// against: the keywords actually used by the Finding schema (spec 002 §3.3) —
// type, const, required, additionalProperties, properties, items, minLength,
// maxLength, minimum, maximum, maxItems, and the date-time format.
//
// It is hand-rolled on purpose (CLAUDE.md: stdlib first). A general-purpose
// validator dependency would buy draft-2020 features this schema does not use;
// unsupported keywords here fail loudly at load time rather than passing
// silently, so the schema cannot outgrow the validator unnoticed.
type Schema struct {
	Type                 string             `json:"type"`
	Const                *json.RawMessage   `json:"const"`
	Format               string             `json:"format"`
	Required             []string           `json:"required"`
	AdditionalProperties *bool              `json:"additionalProperties"`
	Properties           map[string]*Schema `json:"properties"`
	Items                *Schema            `json:"items"`
	MinLength            *int               `json:"minLength"`
	MaxLength            *int               `json:"maxLength"`
	Minimum              *float64           `json:"minimum"`
	Maximum              *float64           `json:"maximum"`
	MinItems             *int               `json:"minItems"`
	MaxItems             *int               `json:"maxItems"`

	// ID and Description are carried for round-tripping and documentation;
	// neither affects validation.
	ID          string `json:"$id"`
	Description string `json:"description"`
}

// supportedKeywords is every keyword ParseSchema understands. Anything else
// in a schema document is an error: a silently ignored constraint is a
// validation hole.
var supportedKeywords = map[string]bool{
	"type": true, "const": true, "format": true, "required": true,
	"additionalProperties": true, "properties": true, "items": true,
	"minLength": true, "maxLength": true, "minimum": true, "maximum": true,
	"minItems": true, "maxItems": true, "$id": true, "description": true,
}

// ParseSchema decodes a JSON Schema document into the supported subset. It
// fails on any keyword the validator does not implement.
func ParseSchema(doc []byte) (*Schema, error) {
	var s Schema
	if err := json.Unmarshal(doc, &s); err != nil {
		return nil, fmt.Errorf("decoding schema: %w", err)
	}
	if err := checkKeywords(doc, "#"); err != nil {
		return nil, err
	}
	return &s, nil
}

// checkKeywords walks the raw document rejecting unsupported keywords, so an
// edit to the checked-in schema that this validator cannot enforce is caught
// at load time (and therefore in tests) rather than at runtime.
func checkKeywords(doc []byte, path string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(doc, &raw); err != nil {
		return fmt.Errorf("decoding schema at %s: %w", path, err)
	}
	for _, k := range sortedKeys(raw) {
		if !supportedKeywords[k] {
			return fmt.Errorf("schema at %s: unsupported keyword %q", path, k)
		}
	}
	if props, ok := raw["properties"]; ok {
		var sub map[string]json.RawMessage
		if err := json.Unmarshal(props, &sub); err != nil {
			return fmt.Errorf("decoding properties at %s: %w", path, err)
		}
		for _, name := range sortedKeys(sub) {
			if err := checkKeywords(sub[name], path+"/"+name); err != nil {
				return err
			}
		}
	}
	if items, ok := raw["items"]; ok {
		if err := checkKeywords(items, path+"/items"); err != nil {
			return err
		}
	}
	return nil
}

// ValidationError is one constraint violation, located by a JSON path such as
// "evidence[2].query".
type ValidationError struct {
	// Path locates the offending value in the validated document. The empty
	// path means the document root.
	Path string
	// Message describes the violated constraint in terms the model can act on.
	Message string
}

// Error renders the violation as "path: message".
func (e ValidationError) Error() string {
	if e.Path == "" {
		return e.Message
	}
	return e.Path + ": " + e.Message
}

// ValidationErrors is an ordered set of constraint violations. Ordering is
// deterministic (document order, properties visited alphabetically) so the
// repair feedback handed to the model is reproducible.
type ValidationErrors []ValidationError

// Error renders every violation, one per line.
func (v ValidationErrors) Error() string {
	parts := make([]string, len(v))
	for i, e := range v {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// Validate checks a JSON document against the schema and returns every
// violation found. A nil return means the document is valid.
func (s *Schema) Validate(doc []byte) ValidationErrors {
	var value any
	if err := json.Unmarshal(doc, &value); err != nil {
		return ValidationErrors{{Message: fmt.Sprintf("not valid JSON: %v", err)}}
	}
	return s.validate(value, "")
}

// validate applies the schema to an already-decoded JSON value at path.
func (s *Schema) validate(value any, path string) ValidationErrors {
	var errs ValidationErrors
	if s.Const != nil {
		var want any
		if err := json.Unmarshal(*s.Const, &want); err == nil && !equalJSON(want, value) {
			errs = append(errs, ValidationError{path, fmt.Sprintf("must be %s", string(*s.Const))})
			return errs
		}
	}
	switch s.Type {
	case "":
		// No type constraint (e.g. a const-only schema).
	case "object":
		m, ok := value.(map[string]any)
		if !ok {
			return append(errs, ValidationError{path, fmt.Sprintf("must be an object, got %s", jsonKind(value))})
		}
		errs = append(errs, s.validateObject(m, path)...)
	case "array":
		a, ok := value.([]any)
		if !ok {
			return append(errs, ValidationError{path, fmt.Sprintf("must be an array, got %s", jsonKind(value))})
		}
		errs = append(errs, s.validateArray(a, path)...)
	case "string":
		str, ok := value.(string)
		if !ok {
			return append(errs, ValidationError{path, fmt.Sprintf("must be a string, got %s", jsonKind(value))})
		}
		errs = append(errs, s.validateString(str, path)...)
	case "number", "integer":
		n, ok := value.(float64)
		if !ok {
			return append(errs, ValidationError{path, fmt.Sprintf("must be a %s, got %s", s.Type, jsonKind(value))})
		}
		if s.Type == "integer" && n != math.Trunc(n) {
			errs = append(errs, ValidationError{path, "must be an integer"})
		}
		if s.Minimum != nil && n < *s.Minimum {
			errs = append(errs, ValidationError{path, fmt.Sprintf("must be >= %v, got %v", *s.Minimum, n)})
		}
		if s.Maximum != nil && n > *s.Maximum {
			errs = append(errs, ValidationError{path, fmt.Sprintf("must be <= %v, got %v", *s.Maximum, n)})
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			errs = append(errs, ValidationError{path, fmt.Sprintf("must be a boolean, got %s", jsonKind(value))})
		}
	default:
		errs = append(errs, ValidationError{path, fmt.Sprintf("schema declares unsupported type %q", s.Type)})
	}
	return errs
}

// validateObject enforces required, additionalProperties, and per-property
// subschemas. Properties are visited alphabetically for deterministic output.
func (s *Schema) validateObject(m map[string]any, path string) ValidationErrors {
	var errs ValidationErrors
	for _, name := range s.Required {
		if _, ok := m[name]; !ok {
			errs = append(errs, ValidationError{join(path, name), "is required"})
		}
	}
	for _, name := range sortedKeys(m) {
		sub, ok := s.Properties[name]
		if !ok {
			if s.AdditionalProperties != nil && !*s.AdditionalProperties {
				errs = append(errs, ValidationError{join(path, name), "is not an allowed property"})
			}
			continue
		}
		errs = append(errs, sub.validate(m[name], join(path, name))...)
	}
	return errs
}

// validateArray enforces item counts and the item subschema.
func (s *Schema) validateArray(a []any, path string) ValidationErrors {
	var errs ValidationErrors
	if s.MinItems != nil && len(a) < *s.MinItems {
		errs = append(errs, ValidationError{path, fmt.Sprintf("must have at least %d items, got %d", *s.MinItems, len(a))})
	}
	if s.MaxItems != nil && len(a) > *s.MaxItems {
		errs = append(errs, ValidationError{path, fmt.Sprintf("must have at most %d items, got %d", *s.MaxItems, len(a))})
	}
	if s.Items == nil {
		return errs
	}
	for i, item := range a {
		errs = append(errs, s.Items.validate(item, fmt.Sprintf("%s[%d]", path, i))...)
	}
	return errs
}

// validateString enforces length bounds and the date-time format.
func (s *Schema) validateString(str, path string) ValidationErrors {
	var errs ValidationErrors
	// Lengths are counted in runes: the caps exist to bound what a human and
	// a model read, and a multi-byte character is one of those either way.
	n := len([]rune(str))
	if s.MinLength != nil && n < *s.MinLength {
		errs = append(errs, ValidationError{path, fmt.Sprintf("must be at least %d characters, got %d", *s.MinLength, n)})
	}
	if s.MaxLength != nil && n > *s.MaxLength {
		errs = append(errs, ValidationError{path, fmt.Sprintf("must be at most %d characters, got %d", *s.MaxLength, n)})
	}
	if s.Format == "date-time" {
		if _, err := time.Parse(time.RFC3339, str); err != nil {
			errs = append(errs, ValidationError{path, "must be an RFC 3339 date-time"})
		}
	}
	return errs
}

// join appends a property name to a JSON path.
func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// jsonKind names the JSON type of a decoded value, for error messages.
func jsonKind(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if t == math.Trunc(t) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// equalJSON compares two decoded JSON values for equality. Only scalars are
// compared structurally, which is all const is used for here.
func equalJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// sortedKeys returns a map's keys in lexicographic order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
