package llm

import "fmt"

// TransientError is implemented by errors that can classify themselves as
// retryable. Retry policy asks the error instead of inspecting a provider's
// SDK types, so every provider — Anthropic, Ollama, anything added later —
// reaches the same retry behavior by translating its own failures into a
// value that answers this question (spec 001 §3.3).
type TransientError interface {
	error
	// Transient reports whether retrying the identical request could
	// plausibly succeed.
	Transient() bool
}

// APIError is a provider-agnostic API failure: the backend answered with an
// HTTP status rather than a model response. Providers translate their client
// library's error type into this one at their own boundary, which is the only
// place a provider SDK is imported; middleware and orchestration classify
// failures through it.
type APIError struct {
	// Provider is the name of the provider that produced the error, matching
	// Provider.Name() (e.g. "anthropic", "ollama").
	Provider string
	// StatusCode is the HTTP status the backend returned. Zero means the
	// failure carried no status.
	StatusCode int
	// Message is the backend's error text. Optional: when empty, Error falls
	// back to the wrapped error's message.
	Message string
	// Err is the underlying provider error, retained so callers that need
	// provider specifics can reach it with errors.As. May be nil.
	Err error
}

// Error renders the provider, status, and backend message.
func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	provider := e.Provider
	if provider == "" {
		provider = "llm"
	}
	if msg == "" {
		return fmt.Sprintf("%s api error: HTTP %d", provider, e.StatusCode)
	}
	return fmt.Sprintf("%s api error: HTTP %d: %s", provider, e.StatusCode, msg)
}

// Unwrap returns the underlying provider error so errors.Is and errors.As
// reach provider-specific detail through an APIError.
func (e *APIError) Unwrap() error { return e.Err }

// Transient reports whether the request is worth retrying: rate limits (429)
// and server-side failures (5xx) are, since the same request may succeed
// later. Every other status — the rest of the 4xx family above all — is a
// problem with the request itself and will fail identically on a retry.
func (e *APIError) Transient() bool {
	return e.StatusCode == 429 || e.StatusCode >= 500
}
