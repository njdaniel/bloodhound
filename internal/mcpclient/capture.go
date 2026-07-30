package mcpclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// captureSubdir is the subdirectory under the capture dir that MCP captures
// are written to, matching the work-dir layout in spec 002 §4.2 (LLM
// captures live in llm/, written by internal/llm/middleware).
const captureSubdir = "mcp"

// CaptureRecord is the on-disk schema of one capture file: the tool call and
// either its result or the transport error that ended it.
type CaptureRecord struct {
	Seq    int             `json:"seq"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result *Result         `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// writeCapture marshals rec and writes it to its sequence-numbered file.
func (s *Session) writeCapture(rec CaptureRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling capture record: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(s.captureDir, captureFilename(rec.Seq, rec.Tool))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing capture file: %w", err)
	}
	return nil
}

// captureFilename renders the NNN-<tool>.json capture filename, with the
// tool name sanitized so a hostile server cannot steer writes outside the
// capture dir.
func captureFilename(seq int, tool string) string {
	return fmt.Sprintf("%03d-%s.json", seq, safeName(tool))
}

// safeName replaces every byte outside [A-Za-z0-9._-] with '_' so tool names
// are always path-safe filename components.
func safeName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '_' || r == '-':
			return r
		default:
			return '_'
		}
	}, name)
}
