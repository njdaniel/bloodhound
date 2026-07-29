package middleware

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/njdaniel/bloodhound/internal/llm"
)

// RetryConfig tunes the retry decorator. Zero values take the defaults below.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts, including the first.
	// Default 3.
	MaxAttempts int
	// InitialBackoff is the delay before the first retry; it doubles per
	// attempt. Default 500ms.
	InitialBackoff time.Duration
	// MaxBackoff caps the per-retry delay. Default 8s.
	MaxBackoff time.Duration
}

const (
	defaultMaxAttempts    = 3
	defaultInitialBackoff = 500 * time.Millisecond
	defaultMaxBackoff     = 8 * time.Second
)

// Retry decorates a Provider with exponential backoff on transient API
// errors only (HTTP 429/5xx and network timeouts). Other 4xx errors, context
// cancellation, and deadline expiry are returned immediately.
type Retry struct {
	next llm.Provider
	cfg  RetryConfig
	// sleep waits for d or until ctx is done; swapped out in tests.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewRetry wraps next with retry-on-transient-error behavior.
func NewRetry(next llm.Provider, cfg RetryConfig) *Retry {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = defaultInitialBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	return &Retry{next: next, cfg: cfg, sleep: sleepCtx}
}

// Complete runs the wrapped provider, retrying transient failures with
// exponential backoff until an attempt succeeds, a permanent error occurs,
// the context ends, or attempts are exhausted.
func (r *Retry) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	backoff := r.cfg.InitialBackoff
	var lastErr error
	for attempt := 1; attempt <= r.cfg.MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := r.sleep(ctx, backoff); err != nil {
				return llm.Response{}, fmt.Errorf("waiting to retry after transient error (%w): %w", lastErr, err)
			}
			backoff = min(backoff*2, r.cfg.MaxBackoff)
		}
		resp, err := r.next.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		if !Transient(err) {
			return llm.Response{}, err
		}
		lastErr = err
	}
	return llm.Response{}, fmt.Errorf("giving up after %d attempts: %w", r.cfg.MaxAttempts, lastErr)
}

// CountCost delegates to the wrapped provider.
func (r *Retry) CountCost(u llm.Usage) llm.Cost { return r.next.CountCost(u) }

// Name delegates to the wrapped provider.
func (r *Retry) Name() string { return r.next.Name() }

// Transient reports whether err is a transient API failure worth retrying:
// an Anthropic API error with status 429 or 5xx, or a timeout (any net.Error
// with Timeout() true, which includes per-request HTTP timeouts and
// context.DeadlineExceeded). Other 4xx statuses and deliberate cancellation
// are permanent. When the timeout came from the caller's own context, the
// retry loop still stops immediately: its backoff sleep returns the context's
// error before another attempt is made.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// sleepCtx waits for d, returning early with the context's error if it ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
