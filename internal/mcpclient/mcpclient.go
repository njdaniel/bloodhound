// Package mcpclient manages orchestrator-side MCP sessions: it spawns an MCP
// server binary as a child process, speaks the protocol over stdio via the
// official go-sdk, and captures every tool call and result to the case work
// dir (spec 002 §4.2) as the substrate for evidence references (§3.4) and the
// M4 evidence-honesty check.
//
// The error split mirrors the MCP spec's: tool errors — results the server
// marks IsError, e.g. bad PromQL or a too-wide query window — come back as
// values (Result.IsError) so the hound loop can hand them to the model to
// fix; transport and protocol failures (dead server, timeout, unknown tool)
// come back as Go errors, because those are ours.
//
// One Session per hound; pooling across concurrent hounds is M2.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/njdaniel/bloodhound/internal/seqname"
)

// clientName and clientVersion identify bloodhound to MCP servers during
// initialization.
const (
	clientName    = "bloodhound"
	clientVersion = "0.1.0"
)

// DefaultCallTimeout is the per-tool-call timeout used when Config leaves
// CallTimeout unset. It bounds one CallTool round trip, not the session.
const DefaultCallTimeout = 30 * time.Second

// Config describes how to spawn and talk to one MCP server.
type Config struct {
	// Command is the path to the MCP server binary (e.g. mcp-prom).
	// Required.
	Command string
	// Args are command-line arguments passed to the server binary.
	Args []string
	// Env holds extra KEY=VALUE entries appended to the parent process
	// environment (e.g. "PROM_URL=http://localhost:9090").
	Env []string
	// CaptureDir is the case work dir's captures/ directory. Tool-call
	// captures are written beneath it as mcp/NNN-<tool>.json. Required:
	// captures are the evidence-reference substrate (spec 002 §3.4), so a
	// session without them is useless downstream.
	CaptureDir string
	// CallTimeout bounds each individual tool call. Zero or negative means
	// DefaultCallTimeout.
	CallTimeout time.Duration
}

// ToolDef is one tool definition as listed by the server — the source of
// truth for hound tool definitions (spec 002 §3.2). It is field-for-field
// convertible to llm.Tool: llm.Tool(def).
type ToolDef struct {
	// Name is the tool's wire name, e.g. "query_range".
	Name string `json:"name"`
	// Description is the server-authored, model-facing description.
	Description string `json:"description"`
	// InputSchema is the tool's input JSON Schema, verbatim from the server.
	InputSchema json.RawMessage `json:"input_schema"`
}

// Result is the outcome of one tool call that completed at the protocol
// level. IsError marks a tool error (the model's problem to fix — the hound
// loop feeds Text back as an error tool_result); transport failures never
// produce a Result, they are returned as Go errors instead.
type Result struct {
	// Text is the concatenated text content of the result. mcp-prom-style
	// servers return exactly one text block containing JSON; multiple blocks
	// are joined with newlines and non-text blocks are ignored.
	Text string `json:"text"`
	// IsError reports that the server marked the result as a tool error.
	IsError bool `json:"is_error,omitempty"`
	// CaptureRef is the capture filename for this call relative to the
	// captures/ directory, e.g. "mcp/003-query_range.json" — the value the
	// hound loop injects into Finding evidence (spec 002 §3.4).
	CaptureRef string `json:"capture_ref,omitempty"`
}

// Session is a live connection to one spawned MCP server. It is safe for
// concurrent use; captures are sequence-numbered under a mutex.
type Session struct {
	sess        *mcp.ClientSession
	cmd         *exec.Cmd
	callTimeout time.Duration
	captureDir  string

	mu  sync.Mutex
	seq int

	closeOnce sync.Once
	closeErr  error
	done      chan struct{}
}

// Connect spawns cfg.Command as a child process, connects over stdio, and
// completes the MCP initialize handshake. The returned Session must be
// Closed; if ctx is cancelled first, the session closes itself and the child
// process is terminated — no zombie servers. Capture sequence numbering
// continues from any existing files under cfg.CaptureDir/mcp, so a resumed
// case does not restart at 000 (spec 002 §4.3).
func Connect(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Command == "" {
		return nil, errors.New("mcpclient: Config.Command is required")
	}
	if cfg.CaptureDir == "" {
		return nil, errors.New("mcpclient: Config.CaptureDir is required")
	}
	timeout := cfg.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}

	sub := filepath.Join(cfg.CaptureDir, captureSubdir)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return nil, fmt.Errorf("creating capture dir: %w", err)
	}
	seq, err := seqname.Next(sub)
	if err != nil {
		return nil, fmt.Errorf("scanning capture dir: %w", err)
	}

	// Not CommandContext: that kills the child with SIGKILL the instant ctx
	// ends, which would tear down the stdio transport mid-frame and leave the
	// server no chance to flush. The goroutine below ties the child to ctx
	// through Close instead, which shuts the session down in order.
	cmd := exec.Command(cfg.Command, cfg.Args...) //nolint:noctx // lifetime is tied to ctx via Close below
	cmd.Env = append(os.Environ(), cfg.Env...)
	cmd.Stderr = os.Stderr // surface server diagnostics alongside our own

	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: clientVersion}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to MCP server %s: %w", cfg.Command, err)
	}

	s := &Session{
		sess:        sess,
		cmd:         cmd,
		callTimeout: timeout,
		captureDir:  sub,
		seq:         seq,
		done:        make(chan struct{}),
	}
	// Tie the child's lifetime to ctx: if the caller's context ends before
	// Close is called, shut the session (and therefore the child) down.
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()
	return s, nil
}

// Tools lists the server's tools, following pagination to exhaustion. The
// result is the source of truth for what the hound offers the model.
func (s *Session) Tools(ctx context.Context) ([]ToolDef, error) {
	var defs []ToolDef
	for tool, err := range s.sess.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("listing tools: %w", err)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("marshaling input schema for tool %q: %w", tool.Name, err)
		}
		defs = append(defs, ToolDef{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	return defs, nil
}

// CallTool invokes tool with the given JSON argument object, bounded by the
// per-call timeout. Every call is captured to mcp/NNN-<tool>.json, success or
// failure; a capture write failure fails the call, because an uncaptured call
// cannot be cited as evidence. Tool errors are returned as a Result with
// IsError set and a nil error; transport failures return a non-nil error.
func (s *Session) CallTool(ctx context.Context, tool string, args json.RawMessage) (Result, error) {
	cctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()

	params := &mcp.CallToolParams{Name: tool}
	if len(args) > 0 {
		params.Arguments = args
	}
	res, err := s.sess.CallTool(cctx, params)

	rec := CaptureRecord{Tool: tool, Args: args}
	var out Result
	if err != nil {
		rec.Error = err.Error()
	} else {
		out = Result{Text: contentText(res.Content), IsError: res.IsError}
	}

	s.mu.Lock()
	rec.Seq = s.seq
	s.seq++
	s.mu.Unlock()

	if err == nil {
		out.CaptureRef = path.Join(captureSubdir, seqname.Render(rec.Seq, tool))
		rec.Result = &out
	}

	if werr := s.writeCapture(rec); werr != nil {
		if err != nil {
			return Result{}, fmt.Errorf("calling tool %q: %w (also failed to capture: %w)", tool, err, werr)
		}
		return Result{}, werr
	}
	if err != nil {
		return Result{}, fmt.Errorf("calling tool %q: %w", tool, err)
	}
	return out, nil
}

// Close shuts the session down and terminates the child server process. It
// is idempotent and safe to call concurrently with an in-flight CallTool
// (the call fails with a transport error).
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		if err := s.sess.Close(); err != nil {
			s.closeErr = fmt.Errorf("closing MCP session: %w", err)
		}
	})
	return s.closeErr
}

// contentText concatenates the text content blocks of a tool result,
// newline-joined. Non-text blocks (images, resources) are ignored — every
// bloodhound MCP server returns one text block of JSON.
func contentText(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}
