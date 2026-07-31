package hounds

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/llm/middleware"
	"github.com/njdaniel/bloodhound/internal/mcpclient"
	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// SubmitFindingTool is the name of the synthetic tool a hound calls to end its
// run. It is not served by any MCP server: the loop implements it, and its
// input schema is the Finding schema with capture refs swapped for tool-call
// citations (spec 002 §3.2, §3.4). Termination is forced tool use — the loop
// never parses prose for a conclusion.
const SubmitFindingTool = "submit_finding"

// Budget defaults from spec 002 §3.2. A zero field in the caller's
// orchestrator.Budget takes the default, so budgets stay config-overridable
// without every caller restating them. The values live in the orchestrator
// package, which needs the wall-clock cap to size its investigate phase
// timeout; these are aliases so the two can never drift apart.
const (
	// DefaultMaxToolCalls caps MCP tool calls per hound run.
	DefaultMaxToolCalls = orchestrator.DefaultMaxToolCalls
	// DefaultMaxTokens caps total (input + output) tokens per hound run.
	DefaultMaxTokens = orchestrator.DefaultMaxTokens
	// DefaultMaxWallClock caps a hound run's wall-clock duration.
	DefaultMaxWallClock = orchestrator.DefaultMaxWallClock
	// DefaultMaxOutputTokens caps the output of a single model call.
	DefaultMaxOutputTokens = 4096
)

// MaxRepairAttempts is how many times an invalid submit_finding may be handed
// back to the model with its validation errors before the run fails hard
// (spec 002 §3.2). The third consecutive invalid submission is fatal.
const MaxRepairAttempts = 2

// maxForcedTurns bounds how many turns a run may spend after its budget is
// exhausted: the forced submission plus its repair attempts. Past that, a
// model that keeps ignoring the forced tool choice fails the run instead of
// spending past a budget that has already run out.
const maxForcedTurns = 1 + MaxRepairAttempts

// maxProseTurns bounds the nudge escalation: turn one gets a nudge, turn two
// switches ToolChoice to required, and a third prose-only turn — which a
// conforming provider cannot produce under that choice — fails the run rather
// than spinning until the wall clock does it for us.
const maxProseTurns = 3

// ToolSource is the MCP session surface a hound loop needs.
// *mcpclient.Session satisfies it; tests substitute a fake so no loop test
// spawns a server or touches the network.
type ToolSource interface {
	// Tools lists the server's tool definitions — the source of truth for
	// what the hound offers the model.
	Tools(ctx context.Context) ([]mcpclient.ToolDef, error)
	// CallTool invokes one tool. Tool errors come back as a Result with
	// IsError set; only transport and protocol failures return an error.
	CallTool(ctx context.Context, tool string, args json.RawMessage) (mcpclient.Result, error)
}

// A live MCP session is the production ToolSource; this pins the contract so
// a signature change in mcpclient fails the build here rather than at wiring
// time.
var _ ToolSource = (*mcpclient.Session)(nil)

// Accountant reports accumulated model usage and cost. *middleware.Accounting
// implements it; the loop reads Spend through it instead of decorating the
// provider itself.
type Accountant interface {
	// Totals returns usage and cost across every model call so far.
	Totals() (llm.Usage, llm.Cost)
}

// The middleware accounting decorator is the production Accountant; this pins
// the contract so a signature change there fails the build here.
var _ Accountant = (*middleware.Accounting)(nil)

// Deps carries the collaborators one hound run needs.
//
// The caller owns provider composition and must build the stack in the order
// internal/llm/middleware documents — retry → accounting → capture → provider
// — handing the outermost decorator in as Provider and the accounting layer in
// as Accounting. Compose does exactly that. The loop deliberately does not
// wrap Provider itself: an accounting decorator added here would sit *outside*
// the caller's retry decorator and would count one retried call once instead
// of once per attempt, undercounting Spend on exactly the runs that cost the
// most.
type Deps struct {
	// Provider runs model turns: the outermost decorator of the composed
	// stack. Required.
	Provider llm.Provider
	// Accounting is the accounting layer inside that stack, and the source of
	// the run's token, cost, and budget numbers. Required.
	Accounting Accountant
	// Tools is the hound's MCP session. Required.
	Tools ToolSource
	// Model names the model to invoke. Required.
	Model string
	// MaxOutputTokens caps a single model response. Zero means
	// DefaultMaxOutputTokens.
	MaxOutputTokens int
	// Now reads the clock; nil means time.Now. Tests inject a fake clock to
	// exercise the wall-clock budget deterministically.
	//
	// The loop measures its elapsed time by subtracting two reads, so the
	// values must keep the monotonic clock reading time.Now carries: a clock
	// that normalizes its result, with .UTC() or .Round(0), downgrades the
	// wall-clock budget check to wall-clock arithmetic that a clock step can
	// run backwards (issue #46, and orchestrator.Options.Now for the same
	// hazard one layer up).
	Now func() time.Time
}

// loopConfig is one hound's identity and instructions, everything the shared
// loop needs that differs between hounds.
type loopConfig struct {
	// hound is the hound's short name, matching the Finding "hound" field.
	hound string
	// systemPrompt is the hound's checked-in system prompt asset.
	systemPrompt string
	// deps and budget come from the caller.
	deps   Deps
	budget orchestrator.Budget
}

// runLoop is the shared tool-use loop (spec 002 §3.2): offer the MCP server's
// tools plus submit_finding, execute what the model calls, feed results back,
// and terminate when a valid finding is submitted. opening is the user turn
// that states the case and the focus questions.
func runLoop(ctx context.Context, cfg loopConfig, opening string) (orchestrator.Finding, orchestrator.Spend, error) {
	if cfg.deps.Provider == nil {
		return orchestrator.Finding{}, orchestrator.Spend{}, fmt.Errorf("%s-hound: Deps.Provider is required", cfg.hound)
	}
	if cfg.deps.Accounting == nil {
		return orchestrator.Finding{}, orchestrator.Spend{}, fmt.Errorf("%s-hound: Deps.Accounting is required; compose the provider stack with Compose", cfg.hound)
	}
	if cfg.deps.Tools == nil {
		return orchestrator.Finding{}, orchestrator.Spend{}, fmt.Errorf("%s-hound: Deps.Tools is required", cfg.hound)
	}
	if cfg.deps.Model == "" {
		return orchestrator.Finding{}, orchestrator.Spend{}, fmt.Errorf("%s-hound: Deps.Model is required", cfg.hound)
	}

	now := cfg.deps.Now
	if now == nil {
		now = time.Now
	}
	maxOut := cfg.deps.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = DefaultMaxOutputTokens
	}
	budget := withBudgetDefaults(cfg.budget)
	start := now()
	acct := cfg.deps.Accounting
	provider := cfg.deps.Provider

	// refs holds one capture ref per completed tool call, in call order. The
	// model cites tool calls 1-based; refs[i-1] is call #i (spec 002 §3.4).
	var refs []string
	spend := func() orchestrator.Spend {
		usage, cost := acct.Totals()
		return orchestrator.Spend{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			USD:          cost.USD,
			ToolCalls:    len(refs),
			WallMS:       now().Sub(start).Milliseconds(),
		}
	}
	fail := func(format string, args ...any) (orchestrator.Finding, orchestrator.Spend, error) {
		return orchestrator.Finding{}, spend(), fmt.Errorf(cfg.hound+"-hound: "+format, args...)
	}

	defs, err := cfg.deps.Tools.Tools(ctx)
	if err != nil {
		return fail("listing MCP tools: %w", err)
	}
	tools, err := offeredTools(defs)
	if err != nil {
		return fail("building tool definitions: %w", err)
	}
	served := make(map[string]bool, len(defs))
	for _, d := range defs {
		served[d.Name] = true
	}

	messages := []llm.Message{llm.UserMessage(llm.TextBlock(opening))}
	proseTurns := 0
	invalidSubmits := 0
	forced := false
	forcedTurns := 0

	for {
		// The context is checked first every iteration: the orchestrator owns
		// the phase deadline and cancelling it must stop us promptly.
		if err := ctx.Err(); err != nil {
			return fail("%w", err)
		}

		var choice llm.ToolChoice
		usage, _ := acct.Totals()
		if reason := exhaustedReason(budget, usage, len(refs), now().Sub(start)); reason != "" {
			if !forced {
				messages = append(messages, llm.UserMessage(llm.TextBlock(exhaustionNote(reason))))
				forced = true
			}
			forcedTurns++
			if forcedTurns > maxForcedTurns {
				// A provider that honors ToolChoice cannot get here; refusing
				// to spin past the budget that already ran out is the point.
				return fail("model did not submit a finding within %d forced turns after budget exhaustion", maxForcedTurns)
			}
			choice = llm.ChooseTool(SubmitFindingTool)
		} else if proseTurns >= 2 {
			// Escalation is sticky for the rest of the run: a model that has
			// twice ignored the tools has earned a forced hand.
			choice = llm.ToolChoice{Type: llm.ToolChoiceRequired}
		}

		resp, err := provider.Complete(ctx, llm.Request{
			Model:      cfg.deps.Model,
			System:     cfg.systemPrompt,
			Messages:   messages,
			MaxTokens:  maxOut,
			Tools:      tools,
			ToolChoice: choice,
		})
		if err != nil {
			return fail("model call: %w", err)
		}
		messages = append(messages, resp.Message)

		uses := resp.Message.ToolUses()
		if len(uses) == 0 {
			if forced {
				return fail("model returned prose instead of %s under a forced tool choice", SubmitFindingTool)
			}
			proseTurns++
			if proseTurns >= maxProseTurns {
				return fail("model returned prose with no tool call on %d consecutive turns", proseTurns)
			}
			messages = append(messages, llm.UserMessage(llm.TextBlock(nudgeNote)))
			continue
		}

		var results []llm.ContentBlock
		for _, u := range uses {
			if u.Name == SubmitFindingTool {
				finding, verrs, err := resolveFinding(u.Input, refs)
				if err != nil {
					return fail("validating %s input: %w", SubmitFindingTool, err)
				}
				if len(verrs) == 0 {
					return finding, spend(), nil
				}
				invalidSubmits++
				if invalidSubmits > MaxRepairAttempts {
					return fail("%s input still invalid after %d repair attempts: %w", SubmitFindingTool, MaxRepairAttempts, verrs)
				}
				results = append(results, llm.ToolResultBlock(u.ID, repairNote(verrs), true))
				continue
			}
			if !served[u.Name] {
				results = append(results, llm.ToolResultBlock(u.ID, unknownToolNote(u.Name, defs), true))
				continue
			}
			// Tool errors (bad PromQL, too-wide window) are the model's
			// problem: they come back as Result.IsError and are handed
			// straight to it. Only transport failures are ours.
			res, err := cfg.deps.Tools.CallTool(ctx, u.Name, u.Input)
			if err != nil {
				return fail("calling tool %q: %w", u.Name, err)
			}
			refs = append(refs, res.CaptureRef)
			results = append(results, llm.ToolResultBlock(u.ID, indexedResult(len(refs), res.Text), res.IsError))
		}
		messages = append(messages, llm.UserMessage(results...))
	}
}

// withBudgetDefaults fills unset budget fields with the spec 002 §3.2
// defaults. A negative field means the cap is disabled, which only tests and
// deliberate overrides should ever want.
func withBudgetDefaults(b orchestrator.Budget) orchestrator.Budget { return b.WithDefaults() }

// exhaustedReason reports which cap the run has hit, or "" if it has room
// left. Caps are checked in a fixed order so the note is deterministic.
func exhaustedReason(b orchestrator.Budget, usage llm.Usage, toolCalls int, elapsed time.Duration) string {
	if b.MaxToolCalls > 0 && toolCalls >= b.MaxToolCalls {
		return fmt.Sprintf("tool-call budget reached (%d of %d calls)", toolCalls, b.MaxToolCalls)
	}
	if total := usage.InputTokens + usage.OutputTokens; b.MaxTokens > 0 && total >= b.MaxTokens {
		return fmt.Sprintf("token budget reached (%d of %d tokens)", total, b.MaxTokens)
	}
	if b.MaxWallClock > 0 && elapsed >= b.MaxWallClock {
		return fmt.Sprintf("wall-clock budget reached (%s of %s)", elapsed.Round(time.Second), b.MaxWallClock)
	}
	return ""
}

// exhaustionNote is the user turn appended before the forced final call.
func exhaustionNote(reason string) string {
	return "Budget exhausted: " + reason + ". Submit your best-effort finding now " +
		"with " + SubmitFindingTool + "; unverified theories belong in dead_ends."
}

// nudgeNote is the one-shot correction for a prose-only turn.
const nudgeNote = "That turn contained no tool call. Investigate with the tools, " +
	"and finish by calling " + SubmitFindingTool + " — prose conclusions are not recorded."

// repairNote renders validation errors as the tool_result the model repairs
// from. Errors are listed one per line so a long list stays readable.
func repairNote(errs ValidationErrors) string {
	var b strings.Builder
	b.WriteString(SubmitFindingTool + " was rejected; it did not match the schema:\n")
	for _, e := range errs {
		b.WriteString("- " + e.Error() + "\n")
	}
	b.WriteString("Fix these and call " + SubmitFindingTool + " again.")
	return b.String()
}

// unknownToolNote tells the model it called a tool that does not exist,
// listing what it may call instead.
func unknownToolNote(name string, defs []mcpclient.ToolDef) string {
	available := make([]string, 0, len(defs)+1)
	for _, d := range defs {
		available = append(available, d.Name)
	}
	available = append(available, SubmitFindingTool)
	sort.Strings(available)
	return fmt.Sprintf("unknown tool %q; available tools are: %s", name, strings.Join(available, ", "))
}

// indexedResult prefixes a tool result with its 1-based call number, which is
// how the model cites it as evidence (spec 002 §3.4).
func indexedResult(index int, text string) string {
	return fmt.Sprintf("[tool call #%d]\n%s", index, text)
}

// offeredTools converts the MCP server's tool definitions into llm.Tools and
// appends the synthetic submit_finding tool.
func offeredTools(defs []mcpclient.ToolDef) ([]llm.Tool, error) {
	tools := make([]llm.Tool, 0, len(defs)+1)
	for _, d := range defs {
		if d.Name == SubmitFindingTool {
			return nil, fmt.Errorf("MCP server serves a tool named %q, which collides with the synthetic one", SubmitFindingTool)
		}
		tools = append(tools, llm.Tool(d))
	}
	schema, err := SubmitFindingSchemaJSON()
	if err != nil {
		return nil, err
	}
	tools = append(tools, llm.Tool{
		Name: SubmitFindingTool,
		Description: "Submit your finding and end the investigation. Every evidence " +
			"record must cite the tool call it came from by its [tool call #N] number; " +
			"anything you could not verify belongs in dead_ends.",
		InputSchema: schema,
	})
	return tools, nil
}
