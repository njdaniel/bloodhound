package hounds

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/llm/llmtest"
	"github.com/njdaniel/bloodhound/internal/llm/middleware"
	"github.com/njdaniel/bloodhound/internal/mcpclient"
	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// testDeps builds the Deps a loop test needs: the accounting decorator the
// loop reads Spend from, wrapping the scripted provider. Production wiring
// goes through Compose, which puts retry outside accounting and capture
// inside it; a test needs neither of those layers, only the correct handle.
func testDeps(p llm.Provider, tools ToolSource) Deps {
	acct := middleware.NewAccounting(p)
	return Deps{Provider: acct, Accounting: acct, Tools: tools, Model: "test-model"}
}

// fakeTools is a scripted ToolSource: it serves fixed tool definitions,
// records every call, and returns queued results. No server is spawned and no
// network is touched (spec 002 §5).
type fakeTools struct {
	defs    []mcpclient.ToolDef
	results []mcpclient.Result
	// err, when set, is returned by CallTool as a transport failure.
	err error
	// onCall runs after each recorded call, letting a test advance a clock or
	// cancel a context mid-loop.
	onCall func()

	mu    sync.Mutex
	calls []toolCall
}

// toolCall is one recorded invocation.
type toolCall struct {
	name string
	args json.RawMessage
}

func newFakeTools(results ...mcpclient.Result) *fakeTools {
	return &fakeTools{
		defs: []mcpclient.ToolDef{
			{Name: "query_range", Description: "range query", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "query_instant", Description: "instant query", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		results: results,
	}
}

func (f *fakeTools) Tools(context.Context) ([]mcpclient.ToolDef, error) { return f.defs, nil }

func (f *fakeTools) CallTool(_ context.Context, tool string, args json.RawMessage) (mcpclient.Result, error) {
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, toolCall{name: tool, args: args})
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall()
	}
	if f.err != nil {
		return mcpclient.Result{}, f.err
	}
	if n >= len(f.results) {
		return mcpclient.Result{Text: "{}", CaptureRef: "mcp/999-overflow.json"}, nil
	}
	return f.results[n], nil
}

func (f *fakeTools) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(f.calls))
	for i, c := range f.calls {
		names[i] = c.name
	}
	return names
}

// fakeClock is a manually advanced clock for the wall-clock budget test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// result builds an MCP tool result with a capture ref.
func result(text, ref string) mcpclient.Result {
	return mcpclient.Result{Text: text, CaptureRef: ref}
}

// errorResult builds an MCP tool error — the kind the model is expected to
// fix itself (bad PromQL, too-wide window).
func errorResult(text, ref string) mcpclient.Result {
	return mcpclient.Result{Text: text, IsError: true, CaptureRef: ref}
}

// toolUse builds an assistant response that calls one tool.
func toolUse(id, name, input string, usage llm.Usage) llm.Response {
	return llm.Response{
		Message:    llm.AssistantMessage(llm.ToolUseBlock(llm.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)})),
		StopReason: llm.StopToolUse,
		Usage:      usage,
	}
}

// prose builds an assistant response with no tool call at all.
func prose(text string, usage llm.Usage) llm.Response {
	return llm.Response{
		Message:    llm.AssistantMessage(llm.TextBlock(text)),
		StopReason: llm.StopEndTurn,
		Usage:      usage,
	}
}

// submit builds a submit_finding call carrying the given input.
func submit(id, input string) llm.Response {
	return toolUse(id, SubmitFindingTool, input, llm.Usage{InputTokens: 10, OutputTokens: 5})
}

// validSubmit is a schema-valid submission citing the given tool calls.
func validSubmit(id string, indices ...int) llm.Response {
	records := make([]string, 0, len(indices))
	for _, i := range indices {
		records = append(records, evidenceJSON(i, ""))
	}
	return submit(id, `{"hound":"metrics","summary":"checkout is OOMKilling",`+
		`"confidence":0.8,"evidence":[`+strings.Join(records, ",")+`],`+
		`"dead_ends":[{"theory":"traffic spike","ruled_out_by":"request rate flat"}]}`)
}

// testCase is the fixed Case the loop tests investigate.
func testCase() orchestrator.Case {
	return orchestrator.Case{
		ID:          "c-20260728T101500-a1b2c3",
		AlertName:   "KubePodCrashLooping",
		Labels:      map[string]string{"namespace": "shop", "pod": "checkout-7d9f"},
		FiringSince: time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC),
		WorkDir:     "work/c-20260728T101500-a1b2c3",
	}
}

// lastToolResults returns the tool_result blocks of the final message of req.
func lastToolResults(t *testing.T, req llm.Request) []llm.ToolResult {
	t.Helper()
	if len(req.Messages) == 0 {
		t.Fatal("request has no messages")
	}
	var out []llm.ToolResult
	for _, b := range req.Messages[len(req.Messages)-1].Content {
		if b.Type == llm.BlockToolResult && b.ToolResult != nil {
			out = append(out, *b.ToolResult)
		}
	}
	return out
}

// lastText returns the text of the final message of req.
func lastText(t *testing.T, req llm.Request) string {
	t.Helper()
	if len(req.Messages) == 0 {
		t.Fatal("request has no messages")
	}
	return req.Messages[len(req.Messages)-1].Text()
}

// TestRunHappyPath: two tool calls, then a submission citing both, in order.
func TestRunHappyPath(t *testing.T) {
	tools := newFakeTools(
		result(`{"series":[]}`, "mcp/000-query_range.json"),
		result(`{"samples":[]}`, "mcp/001-query_instant.json"),
	)
	p := llmtest.New(t,
		toolUse("t1", "query_range", `{"query":"up"}`, llm.Usage{InputTokens: 100, OutputTokens: 20}),
		toolUse("t2", "query_instant", `{"query":"up"}`, llm.Usage{InputTokens: 120, OutputTokens: 25}),
		validSubmit("t3", 1, 2),
	)
	p.InputUSDPerMTok, p.OutputUSDPerMTok = 3, 15

	deps := testDeps(p, tools)
	c := testCase()
	finding, spend, err := Run(t.Context(), deps, c, MetricsFocus(c), orchestrator.Budget{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if finding.Hound != MetricsHound || finding.Confidence != 0.8 {
		t.Errorf("finding = %+v, want hound=%s confidence=0.8", finding, MetricsHound)
	}
	wantRefs := []string{"mcp/000-query_range.json", "mcp/001-query_instant.json"}
	if len(finding.Evidence) != 2 {
		t.Fatalf("evidence count = %d, want 2", len(finding.Evidence))
	}
	for i, want := range wantRefs {
		if finding.Evidence[i].CaptureRef != want {
			t.Errorf("evidence[%d].capture_ref = %q, want %q", i, finding.Evidence[i].CaptureRef, want)
		}
	}
	if got := tools.callNames(); len(got) != 2 || got[0] != "query_range" || got[1] != "query_instant" {
		t.Errorf("tool calls = %v, want [query_range query_instant]", got)
	}

	// Spend comes from the accounting decorator, not hand-rolled counters.
	if spend.InputTokens != 230 || spend.OutputTokens != 50 || spend.ToolCalls != 2 {
		t.Errorf("spend = %+v, want 230 in / 50 out / 2 tool calls", spend)
	}
	// Cost accumulates per call, so compare within float tolerance.
	if want := (230*3.0 + 50*15.0) / 1e6; math.Abs(spend.USD-want) > 1e-12 {
		t.Errorf("spend.USD = %v, want %v", spend.USD, want)
	}
	if spend.WallMS < 0 {
		t.Errorf("spend.WallMS = %d, want a non-negative duration", spend.WallMS)
	}

	reqs := p.Requests()
	if len(reqs) != 3 {
		t.Fatalf("model calls = %d, want 3", len(reqs))
	}
	// Tools offered are the session's plus the synthetic one, and the system
	// prompt is the checked-in asset.
	if len(reqs[0].Tools) != 3 || reqs[0].Tools[2].Name != SubmitFindingTool {
		t.Errorf("tools offered = %v, want the 2 MCP tools plus %s", toolNames(reqs[0].Tools), SubmitFindingTool)
	}
	if reqs[0].Tools[0].Description != "range query" {
		t.Errorf("tool description = %q, want the server's own description", reqs[0].Tools[0].Description)
	}
	if reqs[0].System != MetricsSystemPrompt() {
		t.Error("system prompt is not the checked-in asset")
	}
	if reqs[0].ToolChoice.Type != "" {
		t.Errorf("initial tool choice = %q, want auto", reqs[0].ToolChoice.Type)
	}
	if opening := reqs[0].Messages[0].Text(); !strings.Contains(opening, c.AlertName) || !strings.Contains(opening, "namespace: shop") {
		t.Errorf("opening turn does not state the case: %q", opening)
	}
	// Each result is stamped with its 1-based call number — the citation
	// mechanism of §3.4.
	if got := lastToolResults(t, reqs[1]); len(got) != 1 || !strings.HasPrefix(got[0].Content, "[tool call #1]") {
		t.Errorf("first tool result = %+v, want it stamped [tool call #1]", got)
	}
	if got := lastToolResults(t, reqs[2]); len(got) != 1 || !strings.HasPrefix(got[0].Content, "[tool call #2]") {
		t.Errorf("second tool result = %+v, want it stamped [tool call #2]", got)
	}
}

// TestRunBudgetExhaustion covers each cap in spec 002 §3.2: hitting any one
// forces a best-effort submission rather than truncating the run.
func TestRunBudgetExhaustion(t *testing.T) {
	tests := []struct {
		name       string
		budget     orchestrator.Budget
		firstUsage llm.Usage
		advance    time.Duration
		wantNote   string
	}{
		{
			name:       "tool calls",
			budget:     orchestrator.Budget{MaxToolCalls: 1},
			firstUsage: llm.Usage{InputTokens: 10, OutputTokens: 5},
			wantNote:   "tool-call budget reached (1 of 1 calls)",
		},
		{
			name:       "tokens",
			budget:     orchestrator.Budget{MaxTokens: 100},
			firstUsage: llm.Usage{InputTokens: 90, OutputTokens: 20},
			wantNote:   "token budget reached (110 of 100 tokens)",
		},
		{
			name:       "wall clock",
			budget:     orchestrator.Budget{MaxWallClock: 90 * time.Second},
			firstUsage: llm.Usage{InputTokens: 10, OutputTokens: 5},
			advance:    2 * time.Minute,
			wantNote:   "wall-clock budget reached (2m0s of 1m30s)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newFakeClock()
			tools := newFakeTools(result(`{"series":[]}`, "mcp/000-query_range.json"))
			if tt.advance > 0 {
				tools.onCall = func() { clock.advance(tt.advance) }
			}
			p := llmtest.New(t,
				toolUse("t1", "query_range", `{"query":"up"}`, tt.firstUsage),
				validSubmit("t2", 1),
			)
			deps := testDeps(p, tools)
			deps.Now = clock.Now
			c := testCase()

			finding, spend, err := Run(t.Context(), deps, c, MetricsFocus(c), tt.budget)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(finding.Evidence) != 1 {
				t.Errorf("evidence count = %d, want the best-effort finding's 1", len(finding.Evidence))
			}
			if spend.ToolCalls != 1 {
				t.Errorf("spend.ToolCalls = %d, want 1", spend.ToolCalls)
			}
			reqs := p.Requests()
			if len(reqs) != 2 {
				t.Fatalf("model calls = %d, want 2", len(reqs))
			}
			final := reqs[1]
			if final.ToolChoice.Type != llm.ToolChoiceTool || final.ToolChoice.Name != SubmitFindingTool {
				t.Errorf("final tool choice = %+v, want a forced %s", final.ToolChoice, SubmitFindingTool)
			}
			note := lastText(t, final)
			if !strings.Contains(note, tt.wantNote) {
				t.Errorf("exhaustion note = %q, want it to name %q", note, tt.wantNote)
			}
			if !strings.Contains(note, "dead_ends") || !strings.Contains(note, "best-effort") {
				t.Errorf("exhaustion note = %q, want the best-effort/dead_ends instruction", note)
			}
		})
	}
}

// TestRunStopsSpendingAfterExhaustion: a model that ignores the forced tool
// choice does not get to keep querying on an exhausted budget.
func TestRunStopsSpendingAfterExhaustion(t *testing.T) {
	tools := newFakeTools(result(`{"series":[]}`, "mcp/000-query_range.json"))
	query := toolUse("t1", "query_range", `{"query":"up"}`, llm.Usage{InputTokens: 10, OutputTokens: 5})
	p := llmtest.New(t, query, query, query, query)
	deps := testDeps(p, tools)
	c := testCase()

	_, _, err := Run(t.Context(), deps, c, MetricsFocus(c), orchestrator.Budget{MaxToolCalls: 1})
	if err == nil {
		t.Fatal("Run succeeded, want a hard error after the forced-turn limit")
	}
	if !strings.Contains(err.Error(), "forced turns") {
		t.Errorf("error = %v, want it to name the forced-turn limit", err)
	}
	// One call before the budget ran out, then maxForcedTurns forced ones.
	if got, want := len(p.Requests()), 1+maxForcedTurns; got != want {
		t.Errorf("model calls = %d, want %d", got, want)
	}
}

// TestRunRepairsInvalidFinding: validation errors go back as a tool_result
// and the second attempt lands.
func TestRunRepairsInvalidFinding(t *testing.T) {
	tools := newFakeTools(result(`{"series":[]}`, "mcp/000-query_range.json"))
	p := llmtest.New(t,
		toolUse("t1", "query_range", `{"query":"up"}`, llm.Usage{InputTokens: 10, OutputTokens: 5}),
		submit("t2", `{"hound":"metrics","summary":"","confidence":4,"evidence":[],"dead_ends":[]}`),
		validSubmit("t3", 1),
	)
	deps := testDeps(p, tools)
	c := testCase()

	finding, _, err := Run(t.Context(), deps, c, MetricsFocus(c), orchestrator.Budget{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(finding.Evidence) != 1 {
		t.Errorf("evidence count = %d, want 1", len(finding.Evidence))
	}
	reqs := p.Requests()
	if len(reqs) != 3 {
		t.Fatalf("model calls = %d, want 3", len(reqs))
	}
	repair := lastToolResults(t, reqs[2])
	if len(repair) != 1 {
		t.Fatalf("repair tool results = %d, want 1", len(repair))
	}
	if !repair[0].IsError {
		t.Error("repair tool_result is not marked as an error")
	}
	for _, want := range []string{"confidence: must be <= 1", "summary: must be at least 1 characters", SubmitFindingTool} {
		if !strings.Contains(repair[0].Content, want) {
			t.Errorf("repair feedback = %q, want it to contain %q", repair[0].Content, want)
		}
	}
	if repair[0].ToolUseID != "t2" {
		t.Errorf("repair answers tool_use %q, want t2", repair[0].ToolUseID)
	}
}

// TestRunFailsAfterThreeInvalidSubmissions: two repairs is the limit.
func TestRunFailsAfterThreeInvalidSubmissions(t *testing.T) {
	bad := `{"hound":"metrics","summary":"x","confidence":4,"evidence":[],"dead_ends":[]}`
	p := llmtest.New(t, submit("t1", bad), submit("t2", bad), submit("t3", bad))
	deps := testDeps(p, newFakeTools())
	c := testCase()

	_, spend, err := Run(t.Context(), deps, c, MetricsFocus(c), orchestrator.Budget{})
	if err == nil {
		t.Fatal("Run succeeded, want a hard error after the repair limit")
	}
	if !strings.Contains(err.Error(), "still invalid after 2 repair attempts") {
		t.Errorf("error = %v, want it to name the repair limit", err)
	}
	if got := len(p.Requests()); got != 3 {
		t.Errorf("model calls = %d, want 3 (initial + 2 repairs)", got)
	}
	if spend.InputTokens == 0 {
		t.Error("spend is empty on the failure path, want the tokens already burned")
	}
}

// TestRunNudgesThenForcesToolUse: prose once earns a nudge, twice earns
// ToolChoice=required.
func TestRunNudgesThenForcesToolUse(t *testing.T) {
	p := llmtest.New(t,
		prose("Here is my analysis: the pod is unhealthy.", llm.Usage{InputTokens: 10, OutputTokens: 5}),
		prose("To summarize, memory pressure is likely.", llm.Usage{InputTokens: 10, OutputTokens: 5}),
		validSubmit("t3"),
	)
	deps := testDeps(p, newFakeTools())
	c := testCase()

	if _, _, err := Run(t.Context(), deps, c, MetricsFocus(c), orchestrator.Budget{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reqs := p.Requests()
	if len(reqs) != 3 {
		t.Fatalf("model calls = %d, want 3", len(reqs))
	}
	if reqs[1].ToolChoice.Type != "" {
		t.Errorf("tool choice after the first offense = %q, want auto (a nudge, not force)", reqs[1].ToolChoice.Type)
	}
	if nudge := lastText(t, reqs[1]); !strings.Contains(nudge, "no tool call") || !strings.Contains(nudge, SubmitFindingTool) {
		t.Errorf("nudge = %q, want the investigate-with-tools correction", nudge)
	}
	if reqs[2].ToolChoice.Type != llm.ToolChoiceRequired {
		t.Errorf("tool choice after the second offense = %q, want %q", reqs[2].ToolChoice.Type, llm.ToolChoiceRequired)
	}
}

// TestRunSurfacesMCPToolErrors: a tool error is the model's problem to fix,
// so it comes back as a tool_result and the loop keeps going.
func TestRunSurfacesMCPToolErrors(t *testing.T) {
	tools := newFakeTools(
		errorResult("range 36h exceeds 24h limit; narrow start/end", "mcp/000-query_range.json"),
		result(`{"series":[]}`, "mcp/001-query_range.json"),
	)
	p := llmtest.New(t,
		toolUse("t1", "query_range", `{"query":"up","start":"a","end":"b"}`, llm.Usage{InputTokens: 10, OutputTokens: 5}),
		toolUse("t2", "query_range", `{"query":"up","start":"a","end":"c"}`, llm.Usage{InputTokens: 10, OutputTokens: 5}),
		validSubmit("t3", 1, 2),
	)
	deps := testDeps(p, tools)
	c := testCase()

	finding, spend, err := Run(t.Context(), deps, c, MetricsFocus(c), orchestrator.Budget{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spend.ToolCalls != 2 {
		t.Errorf("spend.ToolCalls = %d, want 2 — the failed call counts", spend.ToolCalls)
	}
	res := lastToolResults(t, p.Requests()[1])
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("tool result = %+v, want one marked as an error", res)
	}
	if !strings.Contains(res[0].Content, "exceeds 24h limit") {
		t.Errorf("tool result = %q, want the server's actionable message", res[0].Content)
	}
	// A failed call is still captured, so it remains citable evidence.
	if finding.Evidence[0].CaptureRef != "mcp/000-query_range.json" {
		t.Errorf("evidence[0].capture_ref = %q, want the errored call's capture", finding.Evidence[0].CaptureRef)
	}
}

// TestRunTransportFailureIsFatal: unlike a tool error, a dead session is ours,
// not the model's.
func TestRunTransportFailureIsFatal(t *testing.T) {
	tools := newFakeTools()
	tools.err = errors.New("broken pipe")
	p := llmtest.New(t, toolUse("t1", "query_range", `{"query":"up"}`, llm.Usage{}))
	deps := testDeps(p, tools)
	c := testCase()

	_, _, err := Run(t.Context(), deps, c, MetricsFocus(c), orchestrator.Budget{})
	if err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("err = %v, want the transport failure surfaced", err)
	}
}

// TestRunStopsOnContextCancellation: the context is checked first every
// iteration, so cancelling mid-loop exits before the next model call.
func TestRunStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	tools := newFakeTools(result(`{"series":[]}`, "mcp/000-query_range.json"))
	tools.onCall = cancel
	p := llmtest.New(nil,
		toolUse("t1", "query_range", `{"query":"up"}`, llm.Usage{InputTokens: 10, OutputTokens: 5}),
		validSubmit("t2", 1),
	)
	deps := testDeps(p, tools)
	c := testCase()

	_, spend, err := Run(ctx, deps, c, MetricsFocus(c), orchestrator.Budget{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := len(p.Requests()); got != 1 {
		t.Errorf("model calls = %d, want 1 — the loop must not call again after cancellation", got)
	}
	if p.Remaining() != 1 {
		t.Errorf("queued responses remaining = %d, want the unused submission left over", p.Remaining())
	}
	if spend.ToolCalls != 1 {
		t.Errorf("spend.ToolCalls = %d, want the one call made before cancellation", spend.ToolCalls)
	}
}

// TestRunRejectsUnknownTool: a hallucinated tool name is a correctable
// mistake, not a crash.
func TestRunRejectsUnknownTool(t *testing.T) {
	p := llmtest.New(t,
		toolUse("t1", "query_logs", `{}`, llm.Usage{InputTokens: 10, OutputTokens: 5}),
		validSubmit("t2"),
	)
	tools := newFakeTools()
	deps := testDeps(p, tools)
	c := testCase()

	if _, spend, err := Run(t.Context(), deps, c, MetricsFocus(c), orchestrator.Budget{}); err != nil {
		t.Fatalf("Run: %v", err)
	} else if spend.ToolCalls != 0 {
		t.Errorf("spend.ToolCalls = %d, want 0 — the unknown tool was never called", spend.ToolCalls)
	}
	res := lastToolResults(t, p.Requests()[1])
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Content, `unknown tool "query_logs"`) {
		t.Fatalf("tool result = %+v, want the unknown-tool correction", res)
	}
	if !strings.Contains(res[0].Content, "query_range") {
		t.Errorf("tool result = %q, want it to list the available tools", res[0].Content)
	}
}

// TestRunRequiresDeps checks the guard rails on the entrypoint.
func TestRunRequiresDeps(t *testing.T) {
	c := testCase()
	tests := []struct {
		name string
		deps Deps
	}{
		{"no provider", Deps{Tools: newFakeTools(), Model: "m"}},
		{"no tools", Deps{Provider: llmtest.New(nil), Model: "m"}},
		{"no model", Deps{Provider: llmtest.New(nil), Tools: newFakeTools()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := Run(t.Context(), tt.deps, c, "focus", orchestrator.Budget{}); err == nil {
				t.Fatal("Run succeeded with missing dependencies")
			}
		})
	}
}

// TestMetricsFocusIsMechanical pins the M1 focus derivation: alert facts in,
// focus questions out, no model call.
func TestMetricsFocusIsMechanical(t *testing.T) {
	focus := MetricsFocus(testCase())
	for _, want := range []string{"KubePodCrashLooping", "onset", `namespace="shop"`, `pod="checkout-7d9f"`} {
		if !strings.Contains(focus, want) {
			t.Errorf("focus = %q, want it to contain %q", focus, want)
		}
	}
	if focus != MetricsFocus(testCase()) {
		t.Error("MetricsFocus is not deterministic")
	}
}

// toolNames lists tool names for failure messages.
func toolNames(tools []llm.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}
