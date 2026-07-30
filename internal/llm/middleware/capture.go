package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/njdaniel/bloodhound/internal/llm"
)

// captureSubdir is the subdirectory under the capture dir that LLM captures
// are written to, matching the work-dir layout in spec 002 §4.2.
const captureSubdir = "llm"

// Capture decorates a Provider by writing each request/response pair to a
// sequence-numbered JSON file, llm/NNN-<label>.json, under the configured
// capture directory (the case work dir's captures/ directory). Place it
// innermost so each file records exactly what went over the wire, one file
// per attempt (see the package comment). Captures are the replay and
// evidence-reference substrate, so a capture write failure fails the call.
// Safe for concurrent use.
type Capture struct {
	next  llm.Provider
	dir   string
	label string

	mu  sync.Mutex
	seq int
}

// CaptureRecord is the on-disk schema of one capture file.
type CaptureRecord struct {
	Seq      int           `json:"seq"`
	Label    string        `json:"label"`
	Provider string        `json:"provider"`
	Request  llm.Request   `json:"request"`
	Response *llm.Response `json:"response,omitempty"`
	Error    string        `json:"error,omitempty"`
}

// NewCapture wraps next with request/response capture. Files are written to
// dir/llm/, which is created if needed. label names the caller (e.g.
// "metrics-hound") and becomes part of each filename, so it must be
// path-safe. Sequence numbering continues from any existing capture files in
// the directory, so a resumed case does not restart at 000 (spec 002 §4.3).
func NewCapture(next llm.Provider, dir, label string) (*Capture, error) {
	sub := filepath.Join(dir, captureSubdir)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return nil, fmt.Errorf("creating capture dir: %w", err)
	}
	seq, err := nextSeq(sub)
	if err != nil {
		return nil, fmt.Errorf("scanning capture dir: %w", err)
	}
	return &Capture{next: next, dir: sub, label: label, seq: seq}, nil
}

// Complete runs the wrapped provider and writes one capture file for the
// attempt, recording the request and either the response or the error.
func (c *Capture) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	resp, err := c.next.Complete(ctx, req)

	rec := CaptureRecord{
		Label:    c.label,
		Provider: c.next.Name(),
		Request:  req,
	}
	if err != nil {
		rec.Error = err.Error()
	} else {
		rec.Response = &resp
	}

	c.mu.Lock()
	rec.Seq = c.seq
	c.seq++
	c.mu.Unlock()

	if werr := c.write(rec); werr != nil {
		if err != nil {
			return llm.Response{}, fmt.Errorf("%w (also failed to capture: %w)", err, werr)
		}
		return llm.Response{}, werr
	}
	return resp, err
}

// CountCost delegates to the wrapped provider.
func (c *Capture) CountCost(u llm.Usage) llm.Cost { return c.next.CountCost(u) }

// Name delegates to the wrapped provider.
func (c *Capture) Name() string { return c.next.Name() }

// write marshals rec and writes it to its sequence-numbered file.
func (c *Capture) write(rec CaptureRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling capture record: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(c.dir, fmt.Sprintf("%03d-%s.json", rec.Seq, c.label))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing capture file: %w", err)
	}
	return nil
}

// nextSeq returns one past the highest sequence number already present in
// dir, or 0 for an empty directory. The sequence prefix is every digit up to
// the first '-', not a fixed three: %03d is a minimum width, so past seq 999
// filenames grow to 1000-<label>.json and a fixed-width parser would skip
// them and restart numbering into a collision.
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
