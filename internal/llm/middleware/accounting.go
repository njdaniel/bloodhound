package middleware

import (
	"context"
	"sync"

	"github.com/njdaniel/bloodhound/internal/llm"
)

// Accounting decorates a Provider with token and cost accumulation. Place it
// inside the retry decorator so every retry attempt is counted (see the
// package comment for the intended order). Safe for concurrent use.
type Accounting struct {
	next llm.Provider

	mu    sync.Mutex
	usage llm.Usage
	cost  llm.Cost
	calls int
}

// NewAccounting wraps next with usage/cost accumulation.
func NewAccounting(next llm.Provider) *Accounting {
	return &Accounting{next: next}
}

// Complete runs the wrapped provider and adds the attempt's usage and cost to
// the running totals. Failed attempts count as calls; their usage is whatever
// the provider reported (zero for errors with no response).
func (a *Accounting) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	resp, err := a.next.Complete(ctx, req)
	a.mu.Lock()
	a.calls++
	a.usage = a.usage.Add(resp.Usage)
	a.cost.USD += a.next.CountCost(resp.Usage).USD
	a.mu.Unlock()
	return resp, err
}

// CountCost delegates to the wrapped provider.
func (a *Accounting) CountCost(u llm.Usage) llm.Cost { return a.next.CountCost(u) }

// Name delegates to the wrapped provider.
func (a *Accounting) Name() string { return a.next.Name() }

// Totals returns the accumulated usage and cost across all attempts so far.
func (a *Accounting) Totals() (llm.Usage, llm.Cost) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage, a.cost
}

// Calls returns how many Complete attempts have passed through so far.
func (a *Accounting) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}
