// Command bloodhound is the entrypoint for the bloodhound incident-response
// agent pack. See specs/001-build-spec.md for the architecture and
// specs/002-m1-metrics-path.md §4 for the orchestrator this CLI drives.
//
// Exit codes split refusals from faults. 0 is success. 2 is a refusal: the CLI
// declined the command or its inputs, and rerunning it unchanged fails
// identically — a bad invocation, a command this binary does not implement yet
// (serve and replay, which arrive in M2), an unusable alert file, or a case
// that is missing, undecodable, or written by a pipeline version this binary
// does not walk. 1 is a fault: the command tried its work and something under
// it failed — a phase, which leaves a resumable case, or an I/O fault against
// the work dir or stdout, which may leave nothing to resume. See exitCode for
// the full contract and what a retrying wrapper may conclude from each.
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

Exit codes:
  0  ok.
  1  a fault: the command ran and something under it failed. A failed phase
     leaves the case resumable once the cause is fixed — after a budget
     failure, raise --max-tokens and --resume. An I/O fault against the work
     dir or stdout may leave nothing to resume.
  2  a refusal: rerunning this command unchanged fails identically. A bad
     invocation, a command this binary does not implement yet (serve, replay
     — M2), an unusable alert file, or a case that is missing, undecodable,
     or written by a pipeline version this binary does not walk (a case
     recording none is a mismatch too). A refused alert file still leaves a
     case to resume once the file is fixed.
`

// errUsage marks an invocation the CLI refused before doing any work: no
// command, an unknown one, a flag the command does not take, a missing or
// conflicting target, an empty --work. It maps to exit code 2, with the other
// refusals — all of them mean "change something first, rerunning this as-is
// changes nothing". See exitCode for the full contract.
var errUsage = errors.New("usage")

// errNotImplemented marks a command this binary knows the name of but does not
// implement yet: serve and replay, which arrive in M2 (spec 002 §6). It maps to
// exit code 2 rather than to the exit 1 it used to get (issue #45). Nothing
// ran, nothing was written, and no retry of this binary can ever succeed — it
// takes a newer binary — so a wrapper that retries exit 1 would loop on it for
// a whole milestone. It is kept apart from errUsage because the invocation is
// not wrong; it is early.
var errNotImplemented = errors.New("not implemented")

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

// exitCode maps a command error to the process exit status. The distinction a
// retrying wrapper keys on is refusal (2) versus fault (1).
//
// Exit 2 is the closed set enumerated below, and only that set: the CLI
// declined the command or its inputs, and rerunning it unchanged fails
// identically, every time. A wrapper must stop and surface it. The members and
// what each tells an operator to change:
//
//   - errUsage — the invocation. Includes an empty --work, which is a bad
//     invocation that used to reach orchestrator.New and fall out as 1.
//   - errNotImplemented — nothing, yet: serve and replay need an M2 binary.
//   - orchestrator.ErrBadAlert — the alert file.
//   - orchestrator.ErrPipelineMismatch — nothing can change it (issue #24).
//     The orchestrator refuses that case with deliberately no migration path.
//   - orchestrator.ErrNoSuchCase — the case ID; there is no such case.
//   - orchestrator.ErrCorruptCase — the work dir; a case.json or checkpoint
//     that this binary cannot decode will not decode on the next attempt.
//
// Refused does not mean nothing happened. ErrBadAlert opens the case before
// intake reads the file, on purpose, so it is refused and still leaves a case
// to resume once the file is fixed (spec 002 §4.3) — pinned by
// TestBadAlertFileExitsTwoButLeavesAResumableCase.
//
// Exit 1 is the default, and it is a fault rather than a refusal: the command
// tried to do its work and something under it failed. Exactly two shapes reach
// it (issue #45), and they differ in what they leave behind:
//
//   - a phase ran and failed. It wrote a failed checkpoint and left the case at
//     PhaseFailed, so the case is resumable once the cause is fixed.
//     ErrBudgetExhausted is the clean example: raise --max-tokens, resume, and
//     the case completes. That promise is real and specific to this shape.
//   - an I/O fault against the work dir or stdout: a work root that is not a
//     directory, a case file this process may not read, a full disk, a closed
//     pipe. These can fail before any case exists, so this shape promises
//     nothing to resume.
//
// So exit 1 does not on its own mean "there is a case waiting". What it does
// mean is that the failure is contingent on something that can change without a
// different command or a different binary. Every path where retrying is
// definitionally futile is in the exit-2 set above.
func exitCode(err error) int {
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage),
		errors.Is(err, errNotImplemented),
		errors.Is(err, orchestrator.ErrBadAlert),
		errors.Is(err, orchestrator.ErrPipelineMismatch),
		errors.Is(err, orchestrator.ErrNoSuchCase),
		errors.Is(err, orchestrator.ErrCorruptCase):
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
		// case from its captures without paid calls. Until then this is a
		// refusal, not a failure — see errNotImplemented.
		return fmt.Errorf("%w: %q arrives in M2", errNotImplemented, cmd)
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

// checkWorkRoot rejects an empty --work.
//
// orchestrator.New already refuses an empty Options.Root, but that refusal is a
// library invariant and arrives as an ordinary error, so it landed on the exit-1
// default (issue #45). An empty --work is a bad invocation like any other, and
// `cost --work "" <id>` is worse than useless without this check: it resolves
// the case dir relative to the process's cwd and reports whatever it finds, or
// does not find, there. Checking it here puts both commands on the usage arm.
func checkWorkRoot(work string) error {
	if work == "" {
		return fmt.Errorf("%w: --work needs a directory", errUsage)
	}
	return nil
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
