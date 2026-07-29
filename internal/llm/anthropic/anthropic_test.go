package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/njdaniel/bloodhound/internal/llm"
)

// fakeAPI is an httptest-backed stand-in for the Anthropic Messages API. It
// records every request body and serves canned response bodies in order.
type fakeAPI struct {
	t        *testing.T
	server   *httptest.Server
	bodies   []map[string]any
	respond  []string
	statuses []int
	hits     int
}

func newFakeAPI(t *testing.T, responses ...string) *fakeAPI {
	f := &fakeAPI{t: t, respond: responses}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request body is not JSON: %v", err)
		}
		i := f.hits
		f.hits++
		f.bodies = append(f.bodies, body)

		status := http.StatusOK
		if i < len(f.statuses) && f.statuses[i] != 0 {
			status = f.statuses[i]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if i < len(f.respond) {
			io.WriteString(w, f.respond[i])
		}
	}))
	t.Cleanup(f.server.Close)
	return f
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

	body := fake.bodies[0]
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
	body := fake.bodies[1]
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
	choice := fake.bodies[0]["tool_choice"].(map[string]any)
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
	if fake.hits != 0 {
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
	if fake.hits != 1 {
		t.Errorf("hits = %d, want 1 — SDK retries must be disabled (retry policy lives in middleware)", fake.hits)
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
	if fake.bodies[0]["model"] != "claude-override" {
		t.Errorf("model = %v, want claude-override", fake.bodies[0]["model"])
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
