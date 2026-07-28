package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/njdaniel/bloodhound/internal/agents/hounds"
	"github.com/njdaniel/bloodhound/internal/agents/reporter"
	"github.com/njdaniel/bloodhound/internal/llm/middleware"
	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// DefaultWorkRoot is where case work dirs are created when neither -work nor
// BLOODHOUND_WORK says otherwise.
const DefaultWorkRoot = "work"

// DefaultMCPProm is the mcp-prom binary name looked up on PATH when neither
// -mcp-prom nor BLOODHOUND_MCP_PROM gives a path.
const DefaultMCPProm = "mcp-prom"

// hunt runs one case: either a new one from an alert file, or the remainder of
// an existing one from its checkpoints.
func (a *app) hunt(ctx context.Context, args []string) error {
	fs := a.newFlagSet("hunt")
	alert := fs.String("alert", "", "path to an Alertmanager-format alert JSON file")
	resume := fs.String("resume", "", "case ID to continue from its checkpoints")
	work := fs.String("work", a.envOr("BLOODHOUND_WORK", DefaultWorkRoot), "work root directory")
	model := fs.String("model", a.envOr("BLOODHOUND_MODEL", DefaultModel), "model ID")
	inPrice := fs.Float64("input-price", -1, "input token price in $/MTok (default: the model's published rate)")
	outPrice := fs.Float64("output-price", -1, "output token price in $/MTok (default: the model's published rate)")
	mcpProm := fs.String("mcp-prom", a.envOr("BLOODHOUND_MCP_PROM", DefaultMCPProm), "path to the mcp-prom server binary")
	promURL := fs.String("prom-url", a.getenv("PROM_URL"), "Prometheus base URL handed to mcp-prom")
	maxToolCalls := fs.Int("max-tool-calls", orchestrator.DefaultMaxToolCalls, "tool-call budget for the investigation")
	maxTokens := fs.Int("max-tokens", orchestrator.DefaultMaxTokens, "token budget for the investigation")
	maxWall := fs.Duration("max-wall-clock", orchestrator.DefaultMaxWallClock, "wall-clock budget for the investigation")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	switch {
	case *alert == "" && *resume == "":
		return fmt.Errorf("%w: hunt needs --alert <file> or --resume <case-id>", errUsage)
	case *alert != "" && *resume != "":
		return fmt.Errorf("%w: --alert and --resume are mutually exclusive", errUsage)
	case fs.NArg() > 0:
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}

	pcfg := providerConfig{Model: *model}
	pcfg.InputUSDPerMTok, pcfg.OutputUSDPerMTok, _ = pricesFor(*model)
	if *inPrice >= 0 {
		pcfg.InputUSDPerMTok = *inPrice
	}
	if *outPrice >= 0 {
		pcfg.OutputUSDPerMTok = *outPrice
	}

	// The MCP server outlives any single phase: mcpclient.Connect ties the
	// spawned process to the context it is given, so it gets this
	// command-scoped context and not the investigate phase's deadline
	// context, which would kill the server mid-investigation.
	sessionCtx, endSession := context.WithCancel(ctx)
	defer endSession()

	scfg := sessionConfig{Command: *mcpProm}
	if *promURL != "" {
		scfg.Env = append(scfg.Env, "PROM_URL="+*promURL)
	}

	o, err := orchestrator.New(orchestrator.Options{
		Root: *work,
		Budget: orchestrator.Budget{
			MaxToolCalls: *maxToolCalls,
			MaxTokens:    *maxTokens,
			MaxWallClock: *maxWall,
		},
		Investigate: a.investigator(sessionCtx, pcfg, scfg),
		Report:      reporter.Render,
		Now:         a.now,
	})
	if err != nil {
		return err
	}

	var res orchestrator.Result
	if *resume != "" {
		res, err = o.Resume(ctx, *resume)
	} else {
		res, err = o.Hunt(ctx, *alert)
	}
	if err != nil {
		return err
	}
	return a.printReport(res)
}

// investigator returns the investigate phase implementation: metrics-hound,
// wired to a model provider and an MCP session.
//
// sessionCtx governs the MCP server process. The ctx the returned function
// receives is the investigate phase's deadline context and is used only for
// the hound run itself.
func (a *app) investigator(sessionCtx context.Context, pcfg providerConfig, scfg sessionConfig) orchestrator.InvestigateFunc {
	return func(ctx context.Context, c orchestrator.Case, budget orchestrator.Budget) (orchestrator.Finding, orchestrator.Spend, error) {
		captures := filepath.Join(c.WorkDir, orchestrator.CaptureDir)

		raw, err := a.newProvider(pcfg)
		if err != nil {
			return orchestrator.Finding{}, orchestrator.Spend{}, fmt.Errorf("building model provider: %w", err)
		}
		// Compose enforces the documented middleware order — retry →
		// accounting → capture → provider — and hands back the two handles
		// Deps needs. Accounting must sit inside retry so Spend counts every
		// attempt, not just the last one.
		p, acct, err := hounds.Compose(raw, captures, hounds.MetricsLabel, middleware.RetryConfig{})
		if err != nil {
			return orchestrator.Finding{}, orchestrator.Spend{}, err
		}

		scfg.CaptureDir = captures
		sess, err := a.newSession(sessionCtx, scfg)
		if err != nil {
			return orchestrator.Finding{}, orchestrator.Spend{}, fmt.Errorf("connecting to MCP server: %w", err)
		}
		defer func() {
			if cerr := sess.Close(); cerr != nil {
				fmt.Fprintln(a.stderr, "bloodhound: closing MCP session:", cerr)
			}
		}()

		deps := hounds.Deps{
			Provider:   p,
			Accounting: acct,
			Tools:      sess,
			Model:      pcfg.Model,
		}
		return hounds.Run(ctx, deps, c, hounds.MetricsFocus(c), budget)
	}
}

// printReport writes the rendered terminal report to stdout, followed by where
// the artifacts landed.
func (a *app) printReport(res orchestrator.Result) error {
	text, err := os.ReadFile(filepath.Join(res.Case.WorkDir, orchestrator.ReportText))
	if err != nil {
		return fmt.Errorf("reading rendered report: %w", err)
	}
	if _, err := a.stdout.Write(text); err != nil {
		return fmt.Errorf("writing report to stdout: %w", err)
	}
	fmt.Fprintf(a.stdout, "\nwork dir: %s\n", res.Case.WorkDir)
	return nil
}
