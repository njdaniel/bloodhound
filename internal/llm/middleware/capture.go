package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/seqname"
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
// "metrics-hound") and becomes part of each filename; it is sanitized by
// seqname.Render, so a label that is not already a path-safe component is
// rewritten rather than trusted. Sequence numbering continues from any
// existing capture files in the directory, so a resumed case does not restart
// at 000 (spec 002 §4.3).
func NewCapture(next llm.Provider, dir, label string) (*Capture, error) {
	sub := filepath.Join(dir, captureSubdir)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return nil, fmt.Errorf("creating capture dir: %w", err)
	}
	seq, err := seqname.Next(sub)
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
	// seqname.Render sanitizes the label, so a label that is not a path-safe
	// component cannot steer the write outside the capture dir.
	path := filepath.Join(c.dir, seqname.Render(rec.Seq, c.label))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing capture file: %w", err)
	}
	return nil
}
