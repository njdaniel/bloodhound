package middleware

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/llm/llmtest"
)

func TestAccountingAccumulatesUsageAndCost(t *testing.T) {
	inner := llmtest.New(t,
		llm.Response{Usage: llm.Usage{InputTokens: 1000, OutputTokens: 200}},
		llm.Response{Usage: llm.Usage{InputTokens: 3000, OutputTokens: 800}},
	)
	inner.InputUSDPerMTok = 3.0   // $3 / MTok in
	inner.OutputUSDPerMTok = 15.0 // $15 / MTok out

	a := NewAccounting(inner)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := a.Complete(ctx, llm.Request{}); err != nil {
			t.Fatalf("Complete %d: %v", i, err)
		}
	}

	usage, cost := a.Totals()
	if usage != (llm.Usage{InputTokens: 4000, OutputTokens: 1000}) {
		t.Errorf("usage = %+v, want 4000 in / 1000 out", usage)
	}
	// 4000 * 3/1e6 + 1000 * 15/1e6 = 0.012 + 0.015 = 0.027
	if math.Abs(cost.USD-0.027) > 1e-12 {
		t.Errorf("cost = %v, want 0.027", cost.USD)
	}
	if a.Calls() != 2 {
		t.Errorf("calls = %d, want 2", a.Calls())
	}
}

func TestAccountingCountsFailedAttempts(t *testing.T) {
	inner := llmtest.New(nil, llm.Response{Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}})
	a := NewAccounting(inner)
	ctx := context.Background()

	if _, err := a.Complete(ctx, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Queue is now exhausted; the failed attempt must still count as a call
	// but add no usage.
	if _, err := a.Complete(ctx, llm.Request{}); !errors.Is(err, llmtest.ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}

	usage, _ := a.Totals()
	if usage != (llm.Usage{InputTokens: 100, OutputTokens: 10}) {
		t.Errorf("usage = %+v, want the successful attempt only", usage)
	}
	if a.Calls() != 2 {
		t.Errorf("calls = %d, want 2 (failures count)", a.Calls())
	}
}

func TestAccountingDelegatesCostAndName(t *testing.T) {
	inner := llmtest.New(t)
	inner.InputUSDPerMTok = 1.0
	a := NewAccounting(inner)
	if a.Name() != "llmtest" {
		t.Errorf("Name() = %q, want llmtest", a.Name())
	}
	got := a.CountCost(llm.Usage{InputTokens: 1_000_000})
	if math.Abs(got.USD-1.0) > 1e-12 {
		t.Errorf("CountCost = %v, want 1.0", got.USD)
	}
}
