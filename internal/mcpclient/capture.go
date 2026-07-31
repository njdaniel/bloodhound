package mcpclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/njdaniel/bloodhound/internal/seqname"
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
	// seqname.Render sanitizes the tool name, which comes off the wire from
	// the server, so it cannot steer the write outside the capture dir.
	path := filepath.Join(s.captureDir, seqname.Render(rec.Seq, rec.Tool))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing capture file: %w", err)
	}
	return nil
}
