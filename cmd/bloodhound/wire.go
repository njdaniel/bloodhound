package main

import (
	"context"
	"errors"

	"github.com/njdaniel/bloodhound/internal/agents/hounds"
	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/llm/anthropic"
	"github.com/njdaniel/bloodhound/internal/mcpclient"
)

// provider is the model surface the CLI wires; llm.Provider under a local
// name so the seam in app reads as one thing tests replace.
type provider = llm.Provider

// toolSession is a hound tool source with a lifetime the CLI owns.
// *mcpclient.Session satisfies it.
type toolSession interface {
	hounds.ToolSource
	// Close shuts the session down and terminates the server process.
	Close() error
}

// A live MCP session is the production tool session; this pins the contract so
// a signature change in mcpclient fails the build here.
var _ toolSession = (*mcpclient.Session)(nil)

// providerConfig selects the model and its pricing. Pricing is configuration,
// not a constant baked into call sites: it decides what `bloodhound cost`
// reports, so it must be overridable without a rebuild.
type providerConfig struct {
	// Model is the model ID, e.g. "claude-opus-5".
	Model string
	// InputUSDPerMTok and OutputUSDPerMTok price tokens in dollars per
	// million, matching how model pricing is published.
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64
}

// DefaultModel is the model used when neither -model nor BLOODHOUND_MODEL
// names one.
const DefaultModel = "claude-opus-5"

// modelPrices are the published $/MTok rates for the models bloodhound is
// expected to run on. An unlisted model prices at zero unless the operator
// passes -input-price/-output-price, so cost reports stay honest about not
// knowing rather than inventing a number.
var modelPrices = map[string]struct{ in, out float64 }{
	"claude-opus-5":    {in: 5, out: 25},
	"claude-opus-4-8":  {in: 5, out: 25},
	"claude-sonnet-5":  {in: 3, out: 15},
	"claude-haiku-4-5": {in: 1, out: 5},
}

// pricesFor returns the published rates for a model and whether they are known.
func pricesFor(model string) (in, out float64, known bool) {
	p, ok := modelPrices[model]
	return p.in, p.out, ok
}

// newAnthropicProvider builds the Anthropic provider. The API key comes from
// ANTHROPIC_API_KEY via the SDK — never from a flag, and never from a file in
// this repo (CLAUDE.md).
func newAnthropicProvider(cfg providerConfig) (provider, error) {
	return anthropic.New(anthropic.Config{
		Model:            cfg.Model,
		InputUSDPerMTok:  cfg.InputUSDPerMTok,
		OutputUSDPerMTok: cfg.OutputUSDPerMTok,
	}), nil
}

// sessionConfig describes the MCP server backing the hound's tools.
type sessionConfig struct {
	// Command is the path to the MCP server binary, e.g. mcp-prom.
	Command string
	// Env holds extra KEY=VALUE entries for the server process, e.g. PROM_URL.
	Env []string
	// CaptureDir is the case work dir's captures/ directory.
	CaptureDir string
}

// connectSession spawns the MCP server and completes the handshake.
//
// ctx governs the spawned server's lifetime (mcpclient.Connect ties the child
// process to it), so the caller must pass a context scoped to the session —
// never a per-phase timeout context, which would kill the server the moment
// the phase deadline fired.
func connectSession(ctx context.Context, cfg sessionConfig) (toolSession, error) {
	if cfg.Command == "" {
		return nil, errors.New("no mcp-prom binary configured (set -mcp-prom or BLOODHOUND_MCP_PROM)")
	}
	sess, err := mcpclient.Connect(ctx, mcpclient.Config{
		Command:    cfg.Command,
		Env:        cfg.Env,
		CaptureDir: cfg.CaptureDir,
	})
	if err != nil {
		return nil, err
	}
	return sess, nil
}
