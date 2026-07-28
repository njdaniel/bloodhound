package middleware

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/njdaniel/bloodhound/internal/llm"
)

// stubProvider returns scripted per-attempt errors, then a fixed response.
type stubProvider struct {
	mu    sync.Mutex
	errs  []error // errs[i] is attempt i's error; nil means success
	resp  llm.Response
	calls int
}

func (s *stubProvider) Complete(context.Context, llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return llm.Response{}, s.errs[i]
	}
	return s.resp, nil
}

func (s *stubProvider) CountCost(llm.Usage) llm.Cost { return llm.Cost{} }
func (s *stubProvider) Name() string                 { return "stub" }

// timeoutErr fakes a net.Error whose Timeout() is true.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "dial: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func apiErr(status int) error {
	return &sdk.Error{StatusCode: status}
}

func TestTransientClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limited 429", apiErr(429), true},
		{"server error 500", apiErr(500), true},
		{"overloaded 529", apiErr(529), true},
		{"bad request 400", apiErr(400), false},
		{"unauthorized 401", apiErr(401), false},
		{"not found 404", apiErr(404), false},
		{"request too large 413", apiErr(413), false},
		{"wrapped 429", fmt.Errorf("calling api: %w", apiErr(429)), true},
		{"wrapped 400", fmt.Errorf("calling api: %w", apiErr(400)), false},
		{"network timeout", timeoutErr{}, true},
		{"wrapped network timeout", fmt.Errorf("calling api: %w", timeoutErr{}), true},
		{"context canceled", context.Canceled, false},
		// DeadlineExceeded is a timeout (it implements net.Error with
		// Timeout() true); when the caller's own context is dead, the
		// retry loop's ctx-aware sleep stops further attempts anyway.
		{"context deadline is a timeout", context.DeadlineExceeded, true},
		{"plain error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Transient(tt.err); got != tt.want {
				t.Errorf("Transient(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// noSleep replaces the backoff sleep and records requested delays.
func noSleep(delays *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*delays = append(*delays, d)
		return nil
	}
}

func TestRetrySucceedsFirstAttempt(t *testing.T) {
	stub := &stubProvider{resp: llm.Response{StopReason: llm.StopEndTurn}}
	r := NewRetry(stub, RetryConfig{})
	var delays []time.Duration
	r.sleep = noSleep(&delays)

	resp, err := r.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != llm.StopEndTurn {
		t.Errorf("StopReason = %q, want end_turn", resp.StopReason)
	}
	if stub.calls != 1 {
		t.Errorf("calls = %d, want 1", stub.calls)
	}
	if len(delays) != 0 {
		t.Errorf("slept %v, want no sleeps", delays)
	}
}

func TestRetryRecoversFromTransientErrors(t *testing.T) {
	stub := &stubProvider{
		errs: []error{apiErr(429), apiErr(529)},
		resp: llm.Response{StopReason: llm.StopEndTurn},
	}
	r := NewRetry(stub, RetryConfig{MaxAttempts: 3, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second})
	var delays []time.Duration
	r.sleep = noSleep(&delays)

	if _, err := r.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if stub.calls != 3 {
		t.Errorf("calls = %d, want 3", stub.calls)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}
	if len(delays) != len(want) || delays[0] != want[0] || delays[1] != want[1] {
		t.Errorf("delays = %v, want %v", delays, want)
	}
}

func TestRetryBackoffIsCapped(t *testing.T) {
	stub := &stubProvider{
		errs: []error{apiErr(500), apiErr(500), apiErr(500), apiErr(500)},
		resp: llm.Response{},
	}
	r := NewRetry(stub, RetryConfig{MaxAttempts: 5, InitialBackoff: 100 * time.Millisecond, MaxBackoff: 250 * time.Millisecond})
	var delays []time.Duration
	r.sleep = noSleep(&delays)

	if _, err := r.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 250 * time.Millisecond, 250 * time.Millisecond}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Errorf("delays = %v, want %v", delays, want)
	}
}

func TestRetryDoesNotRetryPermanentErrors(t *testing.T) {
	permanent := apiErr(400)
	stub := &stubProvider{errs: []error{permanent}}
	r := NewRetry(stub, RetryConfig{})
	var delays []time.Duration
	r.sleep = noSleep(&delays)

	_, err := r.Complete(context.Background(), llm.Request{})
	if !errors.Is(err, permanent) {
		t.Fatalf("err = %v, want the original permanent error", err)
	}
	if stub.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retries on 4xx)", stub.calls)
	}
}

func TestRetryExhaustsAttempts(t *testing.T) {
	stub := &stubProvider{errs: []error{apiErr(429), apiErr(429), apiErr(429)}}
	r := NewRetry(stub, RetryConfig{MaxAttempts: 3})
	var delays []time.Duration
	r.sleep = noSleep(&delays)

	_, err := r.Complete(context.Background(), llm.Request{})
	if err == nil {
		t.Fatal("Complete succeeded, want exhaustion error")
	}
	var apiE *sdk.Error
	if !errors.As(err, &apiE) || apiE.StatusCode != 429 {
		t.Errorf("err = %v, want wrapped 429", err)
	}
	if stub.calls != 3 {
		t.Errorf("calls = %d, want 3", stub.calls)
	}
}

func TestRetryStopsWhenContextEnds(t *testing.T) {
	stub := &stubProvider{errs: []error{apiErr(429), apiErr(429)}}
	r := NewRetry(stub, RetryConfig{MaxAttempts: 3, InitialBackoff: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // context already dead when the first backoff starts

	_, err := r.Complete(ctx, llm.Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if stub.calls != 1 {
		t.Errorf("calls = %d, want 1 (no attempts after cancel)", stub.calls)
	}
}

func TestRetryDelegatesCostAndName(t *testing.T) {
	stub := &stubProvider{}
	r := NewRetry(stub, RetryConfig{})
	if r.Name() != "stub" {
		t.Errorf("Name() = %q, want stub", r.Name())
	}
	if got := r.CountCost(llm.Usage{InputTokens: 10}); got != (llm.Cost{}) {
		t.Errorf("CountCost = %+v, want zero", got)
	}
}
