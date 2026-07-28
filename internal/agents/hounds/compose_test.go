package hounds

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/llm/middleware"
	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// flakyProvider fails the first n calls with a transient API error, then
// succeeds. Every call — failed or not — reports usage, which is what makes it
// useful for checking that accounting sits inside retry.
type flakyProvider struct {
	mu       sync.Mutex
	failures int
	calls    int
}

func (p *flakyProvider) Complete(context.Context, llm.Request) (llm.Response, error) {
	p.mu.Lock()
	p.calls++
	failed := p.calls <= p.failures
	p.mu.Unlock()

	resp := llm.Response{
		Message:    llm.AssistantMessage(llm.TextBlock("ok")),
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 10},
	}
	if failed {
		// A rate-limited attempt still consumed input tokens upstream; the
		// provider reports what it knows alongside the error.
		return resp, rateLimited()
	}
	return resp, nil
}

// rateLimited builds an Anthropic 429 the retry decorator recognizes as
// transient. The SDK error renders itself from the request and response, so
// both have to be present for the capture middleware to record it.
func rateLimited() error {
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		panic("building a static request: " + err.Error())
	}
	return &sdk.Error{
		StatusCode: http.StatusTooManyRequests,
		Request:    req,
		Response:   &http.Response{StatusCode: http.StatusTooManyRequests},
	}
}

func (p *flakyProvider) CountCost(u llm.Usage) llm.Cost {
	return llm.Cost{USD: float64(u.InputTokens)/1e6 + float64(u.OutputTokens)/1e6}
}

func (p *flakyProvider) Name() string { return "flaky" }

// TestComposeCountsEveryRetriedAttempt pins the middleware order Compose
// exists to enforce: accounting inside retry, capture innermost. An accounting
// decorator placed outside retry would report one call's usage here instead of
// three, undercounting exactly the runs that cost the most.
func TestComposeCountsEveryRetriedAttempt(t *testing.T) {
	dir := t.TempDir()
	raw := &flakyProvider{failures: 2}

	p, acct, err := Compose(raw, dir, MetricsLabel, middleware.RetryConfig{InitialBackoff: time.Nanosecond})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	if _, err := p.Complete(t.Context(), llm.Request{Model: "test-model", MaxTokens: 16}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if raw.calls != 3 {
		t.Fatalf("provider calls = %d, want 3 (two retried failures plus the success)", raw.calls)
	}

	usage, cost := acct.Totals()
	if usage.InputTokens != 300 || usage.OutputTokens != 30 {
		t.Errorf("usage = %+v, want every attempt counted (300 in / 30 out)", usage)
	}
	if cost.USD <= 0 {
		t.Errorf("cost = %v, want the attempts priced", cost.USD)
	}
	if got := acct.Calls(); got != 3 {
		t.Errorf("accounted calls = %d, want 3", got)
	}

	// Capture is innermost: one file per attempt, including the failures.
	entries, err := os.ReadDir(filepath.Join(dir, "llm"))
	if err != nil {
		t.Fatalf("reading captures: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("capture files = %d, want one per attempt", len(entries))
	}
	for i, want := range []string{"000-metrics-hound.json", "001-metrics-hound.json", "002-metrics-hound.json"} {
		if entries[i].Name() != want {
			t.Errorf("capture %d = %q, want %q", i, entries[i].Name(), want)
		}
	}
}

func TestComposeContinuesCaptureNumbering(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "llm"), 0o755); err != nil {
		t.Fatalf("creating capture dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "llm", "004-metrics-hound.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("seeding capture: %v", err)
	}

	p, _, err := Compose(&flakyProvider{}, dir, MetricsLabel, middleware.RetryConfig{})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if _, err := p.Complete(t.Context(), llm.Request{Model: "test-model", MaxTokens: 16}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "llm", "005-metrics-hound.json")); err != nil {
		t.Errorf("capture numbering restarted instead of continuing: %v", err)
	}
}

func TestComposeRejectsIncompleteWiring(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]struct {
		p     llm.Provider
		dir   string
		label string
	}{
		"no provider": {p: nil, dir: dir, label: MetricsLabel},
		"no dir":      {p: &flakyProvider{}, dir: "", label: MetricsLabel},
		"no label":    {p: &flakyProvider{}, dir: dir, label: ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Compose(tt.p, tt.dir, tt.label, middleware.RetryConfig{}); err == nil {
				t.Fatal("Compose succeeded, want a validation error")
			}
		})
	}
}

// TestRunRequiresAccounting guards the other half of the fix: a caller that
// forgets the accounting handle is refused rather than silently reporting a
// zero Spend.
func TestRunRequiresAccounting(t *testing.T) {
	deps := Deps{Provider: &flakyProvider{}, Tools: newFakeTools(), Model: "test-model"}
	_, _, err := Run(t.Context(), deps, testCase(), "focus", orchestrator.Budget{})
	if err == nil {
		t.Fatal("Run succeeded without an accounting handle")
	}
	if !strings.Contains(err.Error(), "Accounting") {
		t.Errorf("error = %v, want it to name the missing Deps.Accounting", err)
	}
}
