package mcpclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// nextSeq returns one past the highest sequence number already present in
// dir, or 0 for an empty directory. Mirrors the LLM capture middleware so
// both capture streams resume identically (spec 002 §4.3). The sequence
// prefix is every digit up to the first '-', not a fixed three: %03d is a
// minimum width, so past seq 999 filenames grow to 1000-<tool>.json and a
// fixed-width parser would skip them and restart numbering into a collision.
func nextSeq(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("reading dir: %w", err)
	}
	next := 0
	for _, e := range entries {
		n, ok := seqPrefix(e.Name())
		if !ok {
			continue
		}
		if n+1 > next {
			next = n + 1
		}
	}
	return next, nil
}

// seqPrefix parses the leading run of digits before the first '-' in a
// capture filename. It reports false for any name that does not start with
// at least one digit followed by '-', and for values above math.MaxInt32: the
// bound keeps nextSeq's n+1 from overflowing on a corrupt or hand-planted
// name, which would wrap negative and silently restart numbering on top of
// existing captures. ~2e9 is headroom no real case will approach.
func seqPrefix(name string) (int, bool) {
	i := strings.IndexByte(name, '-')
	if i <= 0 {
		return 0, false
	}
	digits := name[:i]
	for j := 0; j < len(digits); j++ {
		if digits[j] < '0' || digits[j] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(digits, 10, 32)
	if err != nil {
		return 0, false
	}
	return int(n), true
}
