package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/njdaniel/bloodhound/internal/llm"
)

// fakeAPI is an httptest-backed stand-in for the Anthropic Messages API. It
// records every request body and serves canned response bodies in order.
type fakeAPI struct {
	// tb is the test the handler reports to. It is a testing.TB rather than a
	// *testing.T so TestFakeAPIHandlerDoesNotGoexit can substitute a recorder
	// and observe how the handler reports.
	tb     testing.TB
	server *httptest.Server

	// respond and statuses are the canned response body and HTTP status for
	// request i. Both are set by the test goroutine before it makes its first
	// request and only read by the handler after that, so the request that
	// carries them across goroutines is also what orders the two accesses.
	respond  []string
	statuses []int

	// hits counts requests that reached the handler and hands each one its
	// index. Atomic so count() can read it without taking mu — but the handler
	// still increments it under mu, see below.
	hits atomic.Int64

	// mu guards bodies *and* the index handed out with each increment of hits.
	// Both under one lock, because an atomic alone would buy race-freedom
	// without buying correctness: with the increment and the append as separate
	// steps, two overlapping handlers can take indices 0 and 1 and then append
	// in the opposite order, at which point bodies[i] is not the body of the
	// request that was answered with respond[i].
	//
	// Synchronizing at all is hygiene rather than a fix for a live race:
	// Complete is synchronous, so handler invocations never actually overlap
	// today. But nothing in the fake enforces that, and the first concurrent
	// test to use it would make the race real.
	mu sync.Mutex
	// bodies holds the decoded body of every request, in arrival order:
	// bodies[i] is the request answered with respond[i] and statuses[i].
	bodies []map[string]any
}

func newFakeAPI(tb testing.TB, responses ...string) *fakeAPI {
	tb.Helper()
	f := &fakeAPI{tb: tb, respond: responses}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Everything in here reports with tb.Errorf, never tb.Fatalf. Fatalf
		// calls FailNow, and FailNow's runtime.Goexit is only valid on the
		// goroutine running the test. From this handler it would unwind the
		// handler's goroutine instead — killing the response mid-write without
		// stopping the test, so the client sees a truncated or EOF response
		// and the failure surfaces far from its cause. Report, answer the
		// request, and let the test goroutine fail on the result.
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			tb.Errorf("unexpected path %q", r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			tb.Errorf("reading request body: %v", err)
			http.Error(w, "reading request body", http.StatusInternalServerError)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			tb.Errorf("request body is not JSON: %v", err)
			http.Error(w, "request body is not JSON", http.StatusBadRequest)
			return
		}
		// Taking the index and recording the body at it is one step, so that
		// bodies[i] is this request even if handlers ever overlap.
		f.mu.Lock()
		i := int(f.hits.Add(1)) - 1
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()

		status := http.StatusOK
		if i < len(f.statuses) && f.statuses[i] != 0 {
			status = f.statuses[i]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if i < len(f.respond) {
			io.WriteString(w, f.respond[i]) //nolint:errcheck // a short write shows up as a client-side decode failure
		}
	}))
	tb.Cleanup(f.server.Close)
	return f
}

// count reports how many requests reached the handler.
func (f *fakeAPI) count() int { return int(f.hits.Load()) }

// body returns the decoded body of request i, failing the test if that many
// requests never arrived. Called from the test goroutine, where Fatalf is
// valid — unlike inside the handler.
func (f *fakeAPI) body(i int) map[string]any {
	f.tb.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		f.tb.Fatalf("wanted request %d, but the fake only saw %d", i, len(f.bodies))
	}
	return f.bodies[i]
}

// recordingTB stands in for *testing.T inside newFakeAPI so a test can drive
// the handler's failure paths without failing the real test. Fatalf mimics
// testing.T faithfully — it ends the calling goroutine via runtime.Goexit —
// which is precisely the behaviour the handler must never trigger. Everything
// not overridden here is delegated: the embedded testing.TB is nil, so any
// unlisted method panics rather than silently doing nothing.
type recordingTB struct {
	testing.TB
	t       *testing.T
	mu      sync.Mutex
	errorfs int
	fatalfs int
}

func (tb *recordingTB) Helper() {}

// Cleanup registers against the real test, so the fake's server is still
// closed when the test that owns this recorder finishes.
func (tb *recordingTB) Cleanup(f func()) { tb.t.Cleanup(f) }

func (tb *recordingTB) Errorf(string, ...any) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.errorfs++
}

func (tb *recordingTB) Fatalf(string, ...any) {
	tb.mu.Lock()
	tb.fatalfs++
	tb.mu.Unlock()
	runtime.Goexit()
}

// counts returns how many times the handler reported, by method.
func (tb *recordingTB) counts() (errorfs, fatalfs int) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.errorfs, tb.fatalfs
}

// TestFakeAPIHandlerDoesNotGoexit pins the fix for a bug this file used to
// have: newFakeAPI's handler reported a malformed request with t.Fatalf, and
// Fatalf calls FailNow, whose runtime.Goexit is only valid on the goroutine
// running the test. From an httptest handler it unwound the handler's
// goroutine instead — net/http's connection cleanup ran and dropped the
// connection without a response, while the test itself carried on. The visible
// symptom was an EOF several layers away in the SDK client, which is what made
// the real mistake hard to pin.
//
// The bug lives on the handler's *failure* path, which Complete can never
// reach: the SDK always sends well-formed JSON. So this test posts to the fake
// directly with a body that is not JSON, and checks both ends of the bug — the
// handler must report with Errorf rather than Fatalf, and the client must get a
// complete HTTP response back rather than a dropped connection. Against the old
// code all three assertions below fire: Fatalf once instead of never, Errorf
// never instead of once, and the POST itself failing with exactly the confusing
// downstream EOF that motivated the fix.
//
// The order matters. How the handler reported is checked first, because the
// POST fails against the old code, and reporting that with t.Fatalf would end
// the test before the Fatalf/Errorf counts were ever read — leaving the two
// assertions that name the actual bug dead in precisely the scenario they exist
// for.
func TestFakeAPIHandlerDoesNotGoexit(t *testing.T) {
	tb := &recordingTB{t: t}
	fake := newFakeAPI(tb, endTurnResponse)

	resp, err := http.Post(fake.server.URL+"/v1/messages", "application/json",
		strings.NewReader("this is not JSON"))

	// Reading the counts here is safe without further synchronization: the
	// handler reports before it answers (or, with the bug, before its goroutine
	// unwinds and net/http closes the connection), and either way the POST above
	// has already observed that.
	errorfs, fatalfs := tb.counts()
	if fatalfs != 0 {
		t.Errorf("handler called Fatalf %d times, want 0: FailNow off the test "+
			"goroutine kills the handler, not the test", fatalfs)
	}
	if errorfs != 1 {
		t.Errorf("handler called Errorf %d times, want 1: a malformed request "+
			"must still fail the test", errorfs)
	}

	if err != nil {
		t.Fatalf("POST to the fake: %v — a handler that calls runtime.Goexit "+
			"drops the connection instead of answering", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing actionable on a test client
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Errorf("reading the response body: %v — the handler did not finish writing it", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for a body that is not JSON", resp.StatusCode, http.StatusBadRequest)
	}
}

func (f *fakeAPI) provider(cfg Config) *Provider {
	return New(cfg,
		option.WithBaseURL(f.server.URL),
		option.WithAPIKey("test-key-not-real"),
	)
}

const toolUseResponse = `{
  "id": "msg_01",
  "type": "message",
  "role": "assistant",
  "model": "claude-test-1",
  "content": [
    {"type": "text", "text": "Checking the metric."},
    {"type": "tool_use", "id": "toolu_01", "name": "query_range", "input": {"query": "up"}}
  ],
  "stop_reason": "tool_use",
  "usage": {"input_tokens": 120, "output_tokens": 30}
}`

const endTurnResponse = `{
  "id": "msg_02",
  "type": "message",
  "role": "assistant",
  "model": "claude-test-1",
  "content": [{"type": "text", "text": "The service is down."}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 200, "output_tokens": 15}
}`

func TestCompleteTranslatesRequest(t *testing.T) {
	fake := newFakeAPI(t, toolUseResponse)
	p := fake.provider(Config{Model: "claude-test-1"})

	req := llm.Request{
		System:    "You investigate incidents.",
		MaxTokens: 1024,
		Messages: []llm.Message{
			llm.UserMessage(llm.TextBlock("What broke?")),
		},
		Tools: []llm.Tool{{
			Name:        "query_range",
			Description: "Run a PromQL range query.",
			InputSchema: json.RawMessage(`{"type":"object","required":["query"],"additionalProperties":false,"properties":{"query":{"type":"string"}}}`),
		}},
		ToolChoice: llm.ToolChoice{Type: llm.ToolChoiceRequired},
	}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	body := fake.body(0)
	if body["model"] != "claude-test-1" {
		t.Errorf("model = %v, want claude-test-1 (from config)", body["model"])
	}
	if body["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens = %v, want 1024", body["max_tokens"])
	}
	system := body["system"].([]any)[0].(map[string]any)
	if system["text"] != "You investigate incidents." {
		t.Errorf("system = %v", system)
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "query_range" {
		t.Errorf("tool name = %v", tool["name"])
	}
	schema := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
	if schema["additionalProperties"] != false {
		t.Errorf("schema additionalProperties = %v, want false (extra keys must survive)", schema["additionalProperties"])
	}
	if got := schema["required"].([]any)[0]; got != "query" {
		t.Errorf("schema required = %v, want [query]", schema["required"])
	}
	if _, ok := schema["properties"].(map[string]any)["query"]; !ok {
		t.Errorf("schema properties missing query: %v", schema["properties"])
	}
	choice := body["tool_choice"].(map[string]any)
	if choice["type"] != "any" {
		t.Errorf(`tool_choice type = %v, want "any" (required maps to any)`, choice["type"])
	}
}

func TestCompleteTranslatesToolUseResponse(t *testing.T) {
	fake := newFakeAPI(t, toolUseResponse)
	p := fake.provider(Config{Model: "claude-test-1"})

	resp, err := p.Complete(context.Background(), llm.Request{
		MaxTokens: 64,
		Messages:  []llm.Message{llm.UserMessage(llm.TextBlock("go"))},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if resp.Usage != (llm.Usage{InputTokens: 120, OutputTokens: 30}) {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if got := resp.Message.Text(); got != "Checking the metric." {
		t.Errorf("Text = %q", got)
	}
	uses := resp.Message.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("ToolUses = %d, want 1", len(uses))
	}
	if uses[0].ID != "toolu_01" || uses[0].Name != "query_range" {
		t.Errorf("ToolUse = %+v", uses[0])
	}
	var input struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(uses[0].Input, &input); err != nil {
		t.Fatalf("unmarshaling tool input: %v", err)
	}
	if input.Query != "up" {
		t.Errorf("tool input query = %q, want up", input.Query)
	}
}

// TestToolUseRoundTrip drives the full shape of the loop against the fake:
// request with tools → tool_use response → tool_result follow-up → end_turn.
func TestToolUseRoundTrip(t *testing.T) {
	fake := newFakeAPI(t, toolUseResponse, endTurnResponse)
	p := fake.provider(Config{Model: "claude-test-1"})
	ctx := context.Background()

	req := llm.Request{
		MaxTokens: 1024,
		Messages:  []llm.Message{llm.UserMessage(llm.TextBlock("What broke?"))},
		Tools: []llm.Tool{{
			Name:        "query_range",
			Description: "Run a PromQL range query.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
		}},
	}
	resp1, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if resp1.StopReason != llm.StopToolUse {
		t.Fatalf("turn 1 StopReason = %q, want tool_use", resp1.StopReason)
	}

	use := resp1.Message.ToolUses()[0]
	req.Messages = append(req.Messages,
		resp1.Message,
		llm.UserMessage(llm.ToolResultBlock(use.ID, `{"series":[]}`, false)),
	)
	resp2, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if resp2.StopReason != llm.StopEndTurn {
		t.Errorf("turn 2 StopReason = %q, want end_turn", resp2.StopReason)
	}

	// The follow-up request must carry the tool_use and tool_result blocks.
	body := fake.body(1)
	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("turn 2 sent %d messages, want 3", len(msgs))
	}
	assistant := msgs[1].(map[string]any)
	aBlocks := assistant["content"].([]any)
	last := aBlocks[len(aBlocks)-1].(map[string]any)
	if last["type"] != "tool_use" || last["id"] != "toolu_01" {
		t.Errorf("assistant tool_use block = %v", last)
	}
	result := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_01" {
		t.Errorf("tool_result block = %v", result)
	}
}

func TestCompleteTranslatesSpecificToolChoice(t *testing.T) {
	fake := newFakeAPI(t, endTurnResponse)
	p := fake.provider(Config{Model: "claude-test-1"})

	_, err := p.Complete(context.Background(), llm.Request{
		MaxTokens:  64,
		Messages:   []llm.Message{llm.UserMessage(llm.TextBlock("submit"))},
		ToolChoice: llm.ChooseTool("submit_finding"),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	choice := fake.body(0)["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "submit_finding" {
		t.Errorf("tool_choice = %v, want tool/submit_finding", choice)
	}
}

func TestCompleteRejectsToolChoiceWithoutName(t *testing.T) {
	fake := newFakeAPI(t)
	p := fake.provider(Config{Model: "claude-test-1"})
	_, err := p.Complete(context.Background(), llm.Request{
		MaxTokens:  64,
		Messages:   []llm.Message{llm.UserMessage(llm.TextBlock("x"))},
		ToolChoice: llm.ToolChoice{Type: llm.ToolChoiceTool},
	})
	if err == nil {
		t.Fatal("Complete succeeded, want error for tool choice without a name")
	}
	if fake.count() != 0 {
		t.Errorf("request went over the wire despite invalid tool choice")
	}
}

func TestCompleteSurfacesAPIErrorsWithoutRetrying(t *testing.T) {
	fake := newFakeAPI(t, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
	fake.statuses = []int{500}
	p := fake.provider(Config{Model: "claude-test-1"})

	_, err := p.Complete(context.Background(), llm.Request{
		MaxTokens: 64,
		Messages:  []llm.Message{llm.UserMessage(llm.TextBlock("x"))},
	})
	if err == nil {
		t.Fatal("Complete succeeded, want API error")
	}
	if fake.count() != 1 {
		t.Errorf("hits = %d, want 1 — SDK retries must be disabled (retry policy lives in middleware)", fake.count())
	}
}

// TestCallerCannotReEnableSDKRetries pins the option ordering in New: the
// WithMaxRetries(0) override is applied after the caller's options, so a
// caller cannot re-enable SDK retries and hide attempts from the retry,
// accounting and capture middleware.
func TestCallerCannotReEnableSDKRetries(t *testing.T) {
	const errBody = `{"type":"error","error":{"type":"api_error","message":"boom"}}`
	fake := newFakeAPI(t, errBody, errBody, errBody, errBody)
	fake.statuses = []int{500, 500, 500, 500}
	p := New(Config{Model: "claude-test-1"},
		option.WithBaseURL(fake.server.URL),
		option.WithAPIKey("test-key-not-real"),
		option.WithMaxRetries(3), // must lose to the provider's own override
	)

	_, err := p.Complete(context.Background(), llm.Request{
		MaxTokens: 64,
		Messages:  []llm.Message{llm.UserMessage(llm.TextBlock("x"))},
	})
	if err == nil {
		t.Fatal("Complete succeeded, want API error")
	}
	if fake.count() != 1 {
		t.Errorf("hits = %d, want 1 — a caller's WithMaxRetries must not re-enable SDK retries", fake.count())
	}
}

// TestCompleteTranslatesAPIErrors pins the provider boundary: the SDK's error
// type stops here, converted into llm.APIError so that retry policy never has
// to know which backend failed. The status must survive the translation — it
// is the whole basis of the transient/permanent decision.
func TestCompleteTranslatesAPIErrors(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantTransient bool
	}{
		{"rate limited", http.StatusTooManyRequests, true},
		{"server error", http.StatusInternalServerError, true},
		{"overloaded", 529, true},
		{"bad request", http.StatusBadRequest, false},
		{"unauthorized", http.StatusUnauthorized, false},
		{"not found", http.StatusNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeAPI(t, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
			fake.statuses = []int{tt.status}
			p := fake.provider(Config{Model: "claude-test-1"})

			_, err := p.Complete(context.Background(), llm.Request{
				MaxTokens: 64,
				Messages:  []llm.Message{llm.UserMessage(llm.TextBlock("x"))},
			})
			var apiErr *llm.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v (%T), want it to carry an *llm.APIError", err, err)
			}
			if apiErr.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tt.status)
			}
			if apiErr.Provider != "anthropic" {
				t.Errorf("Provider = %q, want anthropic", apiErr.Provider)
			}
			if got := apiErr.Transient(); got != tt.wantTransient {
				t.Errorf("Transient() = %v, want %v", got, tt.wantTransient)
			}
			// The SDK error stays reachable underneath: translation adds a
			// provider-agnostic layer, it does not throw detail away.
			var sdkErr *sdk.Error
			if !errors.As(err, &sdkErr) {
				t.Errorf("err = %v, want the underlying *sdk.Error to remain reachable", err)
			}
		})
	}
}

// TestCompleteLeavesNonAPIErrorsAlone: transport failures never reached the
// API, so they carry no status. Labelling them as API errors would invent one.
func TestCompleteLeavesNonAPIErrorsAlone(t *testing.T) {
	// A server that is closed before use: the request fails at dial time.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()

	p := New(Config{Model: "claude-test-1"},
		option.WithBaseURL(url), option.WithAPIKey("test-key-not-real"))
	_, err := p.Complete(context.Background(), llm.Request{
		MaxTokens: 64,
		Messages:  []llm.Message{llm.UserMessage(llm.TextBlock("x"))},
	})
	if err == nil {
		t.Fatal("Complete succeeded against a closed server")
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		t.Errorf("dial failure became %v; a transport error has no HTTP status", apiErr)
	}
}

func TestRequestModelOverridesConfig(t *testing.T) {
	fake := newFakeAPI(t, endTurnResponse)
	p := fake.provider(Config{Model: "claude-default"})
	_, err := p.Complete(context.Background(), llm.Request{
		Model:     "claude-override",
		MaxTokens: 64,
		Messages:  []llm.Message{llm.UserMessage(llm.TextBlock("x"))},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if fake.body(0)["model"] != "claude-override" {
		t.Errorf("model = %v, want claude-override", fake.body(0)["model"])
	}
}

func TestCountCostUsesConfiguredPricing(t *testing.T) {
	p := New(Config{Model: "m", InputUSDPerMTok: 3.0, OutputUSDPerMTok: 15.0},
		option.WithAPIKey("test-key-not-real"))
	got := p.CountCost(llm.Usage{InputTokens: 50_000, OutputTokens: 10_000})
	// 50000*3/1e6 + 10000*15/1e6 = 0.15 + 0.15 = 0.30
	if math.Abs(got.USD-0.30) > 1e-12 {
		t.Errorf("CountCost = %v, want 0.30", got.USD)
	}
	if p.Name() != "anthropic" {
		t.Errorf("Name() = %q, want anthropic", p.Name())
	}
}
