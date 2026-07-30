// Package anthropic_test holds the end-to-end tests for the seam between this
// provider and the retry middleware (issue #26).
//
// Retry classification is a two-step chain: anthropic.translateError turns an
// *sdk.Error into an llm.APIError at the provider boundary, and
// middleware.Transient classifies that llm.APIError. Each half has its own
// unit tests — anthropic_test.go's TestCompleteTranslatesAPIErrors and
// middleware's retry_test.go — but a unit test of either half stays green if
// the other half stops holding up its end. The tests here drive a real
// anthropic.Provider, wrapped in a real middleware.Retry, against an httptest
// server, so the composition itself is pinned.
//
// Why this file lives in an external test package (anthropic_test) under
// internal/llm/anthropic rather than under internal/llm/middleware:
//
//   - Import direction. Both anthropic and middleware import internal/llm;
//     neither imports the other, and that is deliberate — middleware's doc
//     comment states its classification "knows nothing about any provider
//     SDK", and TestRetryRetriesNonAnthropicProvider exists to keep it that
//     way. Adding an anthropic import to middleware's test package would
//     point the dependency the wrong way through the layer it is defending.
//     Pointing it the other way — a provider's tests knowing about the
//     middleware that wraps it — costs nothing, since nothing wraps
//     middleware.
//   - No cycle is possible either way. An external test package is never
//     importable, so anthropic_test importing both anthropic and middleware
//     cannot close a loop, and the non-test anthropic package gains no
//     dependency on middleware at all.
//   - Public surface only. Being outside package anthropic means these tests
//     cannot call translateError directly or reuse the internal fakeAPI
//     helper. That is the point: a seam test that reached into unexported
//     identifiers would stop testing the seam and start testing a half.
package anthropic_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/llm/anthropic"
	"github.com/njdaniel/bloodhound/internal/llm/middleware"
)

// seamOKBody is a minimal successful Messages API response.
const seamOKBody = `{
  "id": "msg_seam",
  "type": "message",
  "role": "assistant",
  "model": "claude-test-1",
  "content": [{"type": "text", "text": "the deployment rolled back"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 200, "output_tokens": 15}
}`

// seamErrorBody is the Messages API's error envelope, served with whatever
// non-2xx status the test asked for.
const seamErrorBody = `{"type":"error","error":{"type":"api_error","message":"boom"}}`

// statusOverloaded is Anthropic's 529 "overloaded" status, which the retry
// policy treats as transient via llm.APIError.Transient's `>= 500` arm.
// net/http has no constant for it. anthropic_test.go spells the same status as
// a bare literal and cannot share this one: that file is in package anthropic
// and this one is not, and a status code that exists only for tests does not
// belong on the non-test package's exported surface.
const statusOverloaded = 529

// seamServer is an httptest stand-in for the Messages API. It serves a fixed
// sequence of HTTP statuses — one per request, falling back to 200 once the
// sequence is exhausted — and counts every request it received.
type seamServer struct {
	// statuses is the status to serve for request i; requests past the end of
	// the slice get 200. Read-only once the server is running.
	statuses []int
	// requests counts requests that reached the handler and hands each one its
	// index. Atomic because the handler runs on the server's goroutines, not
	// the test's — Complete is synchronous, so handler invocations never
	// actually overlap, but nothing in the test enforces that.
	requests atomic.Int64
	// mu guards bodies, which needs more than an atomic can express.
	mu sync.Mutex
	// bodies holds the raw request body of every request, in arrival order.
	bodies []string
	// url is the server's base URL, for option.WithBaseURL.
	url string
}

// recordedBodies returns a copy of the request bodies seen so far.
func (s *seamServer) recordedBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// newSeamServer starts a server that answers the i-th request with
// statuses[i], then 200 for every request after that. It is closed on cleanup.
func newSeamServer(t *testing.T, statuses ...int) *seamServer {
	t.Helper()
	s := &seamServer{statuses: statuses}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Errorf("unexpected path %q, want the messages endpoint", r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		i := int(s.requests.Add(1)) - 1
		s.mu.Lock()
		s.bodies = append(s.bodies, string(raw))
		s.mu.Unlock()

		status := http.StatusOK
		if i < len(s.statuses) {
			status = s.statuses[i]
		}
		body := seamErrorBody
		if status == http.StatusOK {
			body = seamOKBody
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

// retryAround builds the real composition under test: a real
// anthropic.Provider pointed at srv, wrapped in a real middleware.Retry.
//
// The backoff is sub-millisecond on purpose — this runs in the default suite,
// and the middleware's real sleep function is unexported, so shortening the
// configured delay is how the test stays fast without stubbing anything out of
// the path it is meant to exercise.
func retryAround(srv *seamServer, maxAttempts int) *middleware.Retry {
	p := anthropic.New(anthropic.Config{Model: "claude-test-1"},
		option.WithBaseURL(srv.url),
		// A dummy key: the SDK requires one to be set, and the request never
		// leaves the test process.
		option.WithAPIKey("test-key-not-real"),
	)
	return middleware.NewRetry(p, middleware.RetryConfig{
		MaxAttempts:    maxAttempts,
		InitialBackoff: 100 * time.Microsecond,
		MaxBackoff:     time.Millisecond,
	})
}

// seamRequest is the request every test in this file sends.
func seamRequest() llm.Request {
	return llm.Request{
		MaxTokens: 64,
		Messages:  []llm.Message{llm.UserMessage(llm.TextBlock("what broke?"))},
	}
}

// assertRetriedBodiesIdentical checks that srv saw want requests and that
// every one of them carried a byte-identical payload.
//
// A retry is only correct if it re-sends the *same* request: "retry" and
// "send a second, different request" are different operations, and only the
// first is safe to do behind a caller's back. Nothing else in the tree pins
// this. The retry middleware's own stub provider takes its llm.Request as an
// unnamed parameter and discards it, so no middleware test can see what a
// second attempt carried, and the provider's own tests never retry. Without
// this assertion a middleware that mutated llm.Request between attempts —
// dropping messages, zeroing MaxTokens — would still be retrying, still be
// reaching the 200, and still be passing.
func assertRetriedBodiesIdentical(t *testing.T, srv *seamServer, want int) {
	t.Helper()
	bodies := srv.recordedBodies()
	if len(bodies) != want {
		t.Errorf("recorded %d request bodies, want %d", len(bodies), want)
		return
	}
	// The payload must actually be the request, not an empty body that would
	// trivially match itself across attempts.
	if !strings.Contains(bodies[0], "what broke?") {
		t.Errorf("first request body = %s, want it to carry the prompt", bodies[0])
	}
	for i, body := range bodies[1:] {
		if body != bodies[0] {
			t.Errorf("retried request %d carried a different payload:\n attempt 1: %s\n attempt %d: %s",
				i+2, bodies[0], i+2, body)
		}
	}
}

// TestRetryAroundRealProviderRetries503 is the headline seam test: the API
// answers 503 twice and then succeeds, and the composition must ride through
// it. Three requests must reach the server and the third response must come
// back to the caller intact.
//
// This fails if either half of the chain stops holding up its end. If
// translateError stopped producing an llm.APIError, the retry loop would see
// a bare *sdk.Error — which implements neither llm.TransientError nor
// net.Error — and give up after one request. If middleware.Transient stopped
// classifying 5xx as transient, it would give up after one request for the
// other reason. Either way: one request, and an error instead of a response.
func TestRetryAroundRealProviderRetries503(t *testing.T) {
	srv := newSeamServer(t, http.StatusServiceUnavailable, http.StatusServiceUnavailable)

	resp, err := retryAround(srv, 3).Complete(context.Background(), seamRequest())
	if err != nil {
		t.Fatalf("Complete through Retry(anthropic.Provider): %v", err)
	}
	if got := srv.requests.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 (503, 503, 200) — the 503s must be retried through the real provider", got)
	}
	assertRetriedBodiesIdentical(t, srv, 3)
	if resp.StopReason != llm.StopEndTurn {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, llm.StopEndTurn)
	}
	if len(resp.Message.Content) != 1 || resp.Message.Content[0].Text != "the deployment rolled back" {
		t.Errorf("Content = %+v, want the successful response's text", resp.Message.Content)
	}
	if resp.Usage.InputTokens != 200 || resp.Usage.OutputTokens != 15 {
		t.Errorf("Usage = %+v, want {200 15}", resp.Usage)
	}
}

// TestRetryAroundRealProviderClassifiesStatuses widens the headline test
// across the statuses the policy actually distinguishes, so the composition
// cannot pass by retrying everything (or nothing).
//
// Transient statuses must produce three requests and a response; permanent
// ones must produce exactly one request and surface an llm.APIError carrying
// that status. The server serves 200 to any request past the failing one, so a
// permanent status that got retried would show up twice over: a request count
// of 2 and a nil error.
func TestRetryAroundRealProviderClassifiesStatuses(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		wantRequests int64
	}{
		{"rate limited", http.StatusTooManyRequests, 3},
		{"server error", http.StatusInternalServerError, 3},
		{"unavailable", http.StatusServiceUnavailable, 3},
		{"overloaded", statusOverloaded, 3},
		{"bad request", http.StatusBadRequest, 1},
		{"unauthorized", http.StatusUnauthorized, 1},
		{"not found", http.StatusNotFound, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Two failures then success, so a transient status has something
			// to retry into and a permanent one has a success it must not
			// reach.
			srv := newSeamServer(t, tt.status, tt.status)
			transient := tt.wantRequests > 1

			resp, err := retryAround(srv, 3).Complete(context.Background(), seamRequest())
			if got := srv.requests.Load(); got != tt.wantRequests {
				t.Errorf("requests = %d, want %d for HTTP %d", got, tt.wantRequests, tt.status)
			}
			if transient {
				if err != nil {
					t.Fatalf("Complete: %v, want the retry to reach the 200", err)
				}
				if resp.StopReason != llm.StopEndTurn {
					t.Errorf("StopReason = %q, want %q", resp.StopReason, llm.StopEndTurn)
				}
				assertRetriedBodiesIdentical(t, srv, int(tt.wantRequests))
				return
			}
			if err == nil {
				t.Fatalf("Complete succeeded, want HTTP %d returned to the caller unretried", tt.status)
			}
			// The status has to survive the whole composition, not just the
			// translation: it is what a caller above the middleware acts on.
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
		})
	}
}

// TestRetryAroundRealProviderExhaustsAttempts pins the giving-up path: when
// the API never recovers, the composition stops at MaxAttempts rather than
// looping, and the final error still carries the status that caused it.
func TestRetryAroundRealProviderExhaustsAttempts(t *testing.T) {
	srv := newSeamServer(t,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
	)

	_, err := retryAround(srv, 3).Complete(context.Background(), seamRequest())
	if err == nil {
		t.Fatal("Complete succeeded against a server that never recovered")
	}
	if got := srv.requests.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 (MaxAttempts)", got)
	}
	assertRetriedBodiesIdentical(t, srv, 3)
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want it to carry an *llm.APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", apiErr.StatusCode)
	}
}
