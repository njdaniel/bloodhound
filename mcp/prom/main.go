// Command mcp-prom is a standalone MCP server (stdio transport) exposing
// Prometheus to bloodhound's hounds and any other MCP client.
//
// Every tool returns bounded, model-shaped data: capped result sets,
// downsampled series, and explicit truncation markers — never raw dumps.
// Guardrails live in guardrails.go (spec 002 §2.1); the exact truncation
// strategy is spec 002 §2.3.
//
// Configuration (flags override environment):
//
//	PROM_URL      / -prom-url      Prometheus base URL (required)
//	PROM_TIMEOUT  / -prom-timeout  upstream query timeout (default 15s)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-prom:", err)
		os.Exit(1)
	}
}

// run parses configuration and serves MCP over stdio until the client
// disconnects or ctx is cancelled.
func run(ctx context.Context, args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}
	srv := newServer(newPromClient(cfg.promURL, cfg.timeout))
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("serving mcp over stdio: %w", err)
	}
	return nil
}

// config holds the resolved mcp-prom configuration.
type config struct {
	promURL string
	timeout time.Duration
}

// parseConfig resolves configuration from flags and environment; flags win.
func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("mcp-prom", flag.ContinueOnError)
	urlFlag := fs.String("prom-url", "", "Prometheus base URL (overrides PROM_URL)")
	timeoutFlag := fs.String("prom-timeout", "", "upstream query timeout, e.g. 15s (overrides PROM_TIMEOUT)")
	if err := fs.Parse(args); err != nil {
		return config{}, fmt.Errorf("parsing flags: %w", err)
	}

	promURL := *urlFlag
	if promURL == "" {
		promURL = os.Getenv("PROM_URL")
	}
	if promURL == "" {
		return config{}, errors.New("PROM_URL is required (set the PROM_URL environment variable or pass -prom-url)")
	}

	timeout := QueryTimeout
	raw := *timeoutFlag
	if raw == "" {
		raw = os.Getenv("PROM_TIMEOUT")
	}
	if raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return config{}, fmt.Errorf("parsing PROM_TIMEOUT %q: %w", raw, err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("PROM_TIMEOUT must be positive, got %q", raw)
		}
		timeout = d
	}
	return config{promURL: promURL, timeout: timeout}, nil
}
