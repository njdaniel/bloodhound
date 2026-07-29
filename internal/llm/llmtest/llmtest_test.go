package llmtest

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync"
	"testing"

	"github.com/njdaniel/bloodhound/internal/llm"
)

func TestScriptedResponsesServedInOrder(t *testing.T) {
	first := llm.Response{
		Message:    llm.AssistantMessage(llm.ToolUseBlock(llm.ToolUse{ID: "toolu_01", Name: "query_range"})),
		StopReason: llm.StopToolUse,
	}
	second := llm.Response{
		Message:    llm.AssistantMessage(llm.TextBlock("done")),
		StopReason: llm.StopEndTurn,
	}
	p := New(t, first, second)
	ctx := context.Background()

	got1, err := p.Complete(ctx, llm.Request{Model: "m1"})
	if err != nil {
		t.Fatalf("Complete 1: %v", err)
	}
	if got1.StopReason != llm.StopToolUse {
		t.Errorf("first StopReason = %q, want tool_use", got1.StopReason)
	}
	got2, err := p.Complete(ctx, llm.Request{Model: "m2"})
	if err != nil {
		t.Fatalf("Complete 2: %v", err)
	}
	if got2.Message.Text() != "done" {
		t.Errorf("second text = %q, want done", got2.Message.Text())
	}
	if p.Remaining() != 0 {
		t.Errorf("Remaining = %d, want 0", p.Remaining())
	}
}

func TestRecordsEveryRequest(t *testing.T) {
	p := New(t, llm.Response{}, llm.Response{})
	ctx := context.Background()
	if _, err := p.Complete(ctx, llm.Request{Model: "a", MaxTokens: 1}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := p.Complete(ctx, llm.Request{Model: "b", MaxTokens: 2}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	reqs := p.Requests()
	if len(reqs) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(reqs))
	}
	if reqs[0].Model != "a" || reqs[1].Model != "b" {
		t.Errorf("recorded models = %q, %q; want a, b", reqs[0].Model, reqs[1].Model)
	}
}

func TestExhaustedQueueReturnsErrorWithoutTB(t *testing.T) {
	p := New(nil) // no testing.TB: exhaustion must surface as an error
	_, err := p.Complete(context.Background(), llm.Request{})
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
	if len(p.Requests()) != 1 {
		t.Errorf("recorded %d requests, want 1 (exhausted calls are recorded too)", len(p.Requests()))
	}
}

// fakeTB stands in for *testing.T so the exhausted path can be exercised
// without failing the real test. Fatalf mimics testing.T faithfully: it ends
// the calling goroutine via runtime.Goexit.
type fakeTB struct {
	testing.TB
	mu      sync.Mutex
	errorfs int
	fatalfs int
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Errorf(string, ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errorfs++
}

func (f *fakeTB) Fatalf(string, ...any) {
	f.mu.Lock()
	f.fatalfs++
	f.mu.Unlock()
	runtime.Goexit()
}

func (f *fakeTB) counts() (errorfs, fatalfs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.errorfs, f.fatalfs
}

// TestExhaustedQueueReturnsOffTestGoroutine pins the M2 requirement: parallel
// hounds call Complete from worker goroutines, where tb.Fatalf's
// runtime.Goexit would strand the worker mid-call instead of failing the test.
func TestExhaustedQueueReturnsOffTestGoroutine(t *testing.T) {
	tb := &fakeTB{}
	p := New(tb)

	var (
		returned bool
		err      error
	)
	done := make(chan struct{})
	go func() {
		defer close(done) // runs even if Complete calls runtime.Goexit
		_, err = p.Complete(context.Background(), llm.Request{})
		returned = true
	}()
	<-done

	if !returned {
		t.Fatal("Complete never returned: the exhausted path called tb.Fatalf, whose runtime.Goexit is invalid off the test goroutine")
	}
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("err = %v, want ErrExhausted", err)
	}
	errorfs, fatalfs := tb.counts()
	if fatalfs != 0 {
		t.Errorf("tb.Fatalf called %d times, want 0", fatalfs)
	}
	if errorfs != 1 {
		t.Errorf("tb.Errorf called %d times, want 1 (exhaustion must still fail the test)", errorfs)
	}
}

func TestDeterministicPricing(t *testing.T) {
	p := New(t)
	p.InputUSDPerMTok = 3.0
	p.OutputUSDPerMTok = 15.0
	got := p.CountCost(llm.Usage{InputTokens: 1_000_000, OutputTokens: 100_000})
	if math.Abs(got.USD-4.5) > 1e-12 {
		t.Errorf("CountCost = %v, want 4.5", got.USD)
	}
	if p.Name() != "llmtest" {
		t.Errorf("Name() = %q, want llmtest", p.Name())
	}
}
