// Command bloodhound is the entrypoint for the bloodhound incident-response
// agent pack. See specs/001-build-spec.md for the architecture and
// specs/002-m1-metrics-path.md §4 for the orchestrator this CLI drives.
//
// Exit codes: 0 success, 1 a phase failed (the case is resumable once the
// cause is fixed), 2 the invocation or the alert file was unusable.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

var version = "dev"

const usage = `bloodhound — AI incident-response agent pack for Kubernetes

Usage:
  bloodhound hunt --alert <file>   Investigate one Alertmanager alert
  bloodhound hunt --resume <case>  Continue a case from its checkpoints
  bloodhound cost <case-id>        Show the cost breakdown for a case
  bloodhound serve                 Run the webhook intake server (M2)
  bloodhound replay <case-id>      Re-run a case from its captures (M2)
  bloodhound version               Print version

Environment:
  ANTHROPIC_API_KEY   model credentials (never passed as a flag)
  BLOODHOUND_MODEL    default model ID
  BLOODHOUND_WORK     default work root (default "work")
  BLOODHOUND_MCP_PROM path to the mcp-prom server binary
  PROM_URL            Prometheus base URL handed to mcp-prom

Exit codes: 0 ok, 1 phase failure, 2 bad invocation or alert file.
`

// errUsage marks an invocation the CLI refused before doing any work. It maps
// to exit code 2, the same code a bad alert file gets: both mean "fix the
// command, nothing happened".
var errUsage = errors.New("usage")

func main() {
	// Ctrl-C and SIGTERM cancel the run's context: phases stop at their next
	// context check and the current phase records a failed checkpoint, so an
	// interrupted case is resumable rather than lost.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := newApp(os.Stdout, os.Stderr, os.Getenv)
	err := a.run(ctx, os.Args[1:])
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(a.stderr, "bloodhound:", err)
	}
	os.Exit(exitCode(err))
}

// exitCode maps a command error to the process exit status.
func exitCode(err error) int {
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage), errors.Is(err, orchestrator.ErrBadAlert):
		return 2
	default:
		return 1
	}
}

// run dispatches one command. It is the whole of main's behavior, factored out
// so end-to-end tests can drive the CLI in-process with substituted seams.
func (a *app) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usage)
		return fmt.Errorf("%w: no command given", errUsage)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "hunt":
		return a.hunt(ctx, rest)
	case "cost":
		return a.cost(rest)
	case "serve", "replay":
		// M2 (spec 002 §6): serve is the webhook intake, replay re-runs a
		// case from its captures without paid calls.
		return fmt.Errorf("%q is not implemented yet (M2)", cmd)
	case "version":
		fmt.Fprintln(a.stdout, version)
		return nil
	case "-h", "--help", "help":
		fmt.Fprint(a.stdout, usage)
		return nil
	default:
		fmt.Fprint(a.stderr, usage)
		return fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}

// newFlagSet builds a flag set that reports parse errors as usage errors
// instead of exiting the process out from under the caller.
func (a *app) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	return fs
}

// envOr returns the environment variable named key, or fallback when unset.
func (a *app) envOr(key, fallback string) string {
	if v := a.getenv(key); v != "" {
		return v
	}
	return fallback
}

// app carries the process's I/O and the seams tests replace: the environment,
// the clock, the model provider, and the MCP session.
type app struct {
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	now    func() time.Time

	// newProvider builds the raw, undecorated model provider. Middleware is
	// composed around it by hounds.Compose at wiring time.
	newProvider func(cfg providerConfig) (provider, error)
	// newSession spawns and connects to the MCP server that serves hound
	// tools. ctx governs the server process's lifetime, so callers must pass
	// a context that outlives the phase (spec 002 §4.1).
	newSession func(ctx context.Context, cfg sessionConfig) (toolSession, error)
}

// newApp builds the production app: real provider, real MCP session.
func newApp(stdout, stderr io.Writer, getenv func(string) string) *app {
	return &app{
		stdout:      stdout,
		stderr:      stderr,
		getenv:      getenv,
		now:         time.Now,
		newProvider: newAnthropicProvider,
		newSession:  connectSession,
	}
}
