// Package llmtest provides a scripted llm.Provider for tests: it is
// constructed with a queue of canned responses, records every request it
// receives, and marks the test failed (and returns an error) when the queue
// is exhausted. It is the test substrate for every downstream loop test —
// no network, no SDK. See specs/002-m1-metrics-path.md §5.
//
// Every method is safe to call from any goroutine the test outlives,
// including the exhausted path: it reports failure with tb.Errorf, never
// tb.Fatalf, because Fatalf calls runtime.Goexit, which is only valid on the
// goroutine running the test. Parallel hounds call Complete from worker
// goroutines; join those workers before the test function returns. Reporting
// a failure from a goroutine that outlives its test panics ("Fail in
// goroutine after ... has completed") — that is testing's rule, not this
// package's, and no provider can paper over it.
package llmtest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/njdaniel/bloodhound/internal/llm"
)

// ErrExhausted is returned by Complete when the response queue is empty.
var ErrExhausted = errors.New("llmtest: response queue exhausted")

// Provider is a scripted llm.Provider. Each Complete call records the request
// and pops the next canned response off the queue. Safe for concurrent use.
type Provider struct {
	// InputUSDPerMTok and OutputUSDPerMTok set deterministic pricing for
	// CountCost. Both default to zero, making every cost $0.
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64

	tb testing.TB

	mu       sync.Mutex
	queue    []llm.Response
	requests []llm.Request
}

// New builds a scripted provider that serves responses in order. tb may be
// nil; if set, an exhausted queue marks the test failed via tb.Errorf.
// Complete returns ErrExhausted either way.
func New(tb testing.TB, responses ...llm.Response) *Provider {
	if tb != nil {
		tb.Helper()
	}
	return &Provider{tb: tb, queue: append([]llm.Response(nil), responses...)}
}

// Complete records the request and returns the next canned response. An
// exhausted queue marks the test failed with tb.Errorf and returns
// ErrExhausted; it never calls tb.Fatalf, whose runtime.Goexit would be
// invalid on a non-test goroutine and would strand the caller mid-call.
func (p *Provider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	if len(p.queue) == 0 {
		n := len(p.requests)
		p.mu.Unlock()
		if p.tb != nil {
			p.tb.Errorf("llmtest: response queue exhausted after %d requests", n)
		}
		return llm.Response{}, ErrExhausted
	}
	resp := p.queue[0]
	p.queue = p.queue[1:]
	p.mu.Unlock()
	return resp, nil
}

// CountCost prices usage with the provider's deterministic test rates.
func (p *Provider) CountCost(u llm.Usage) llm.Cost {
	return llm.Cost{
		USD: float64(u.InputTokens)*p.InputUSDPerMTok/1e6 +
			float64(u.OutputTokens)*p.OutputUSDPerMTok/1e6,
	}
}

// Name identifies this provider.
func (p *Provider) Name() string { return "llmtest" }

// Requests returns a copy of every request received so far, in order.
func (p *Provider) Requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

// Remaining reports how many canned responses are still queued.
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}
