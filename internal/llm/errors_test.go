package llm

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAPIErrorTransientClassification(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{"rate limited", 429, true},
		{"internal server error", 500, true},
		{"bad gateway", 502, true},
		{"overloaded", 529, true},
		{"bad request", 400, false},
		{"unauthorized", 401, false},
		{"not found", 404, false},
		{"request too large", 413, false},
		{"unprocessable", 422, false},
		{"no status", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &APIError{Provider: "test", StatusCode: tt.status}
			if got := err.Transient(); got != tt.want {
				t.Errorf("(&APIError{StatusCode: %d}).Transient() = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestAPIErrorSatisfiesTransientError(t *testing.T) {
	var err error = &APIError{Provider: "test", StatusCode: 503}
	var classified TransientError
	if !errors.As(err, &classified) {
		t.Fatal("*APIError does not satisfy TransientError")
	}
	if !classified.Transient() {
		t.Error("503 classified as permanent")
	}
}

func TestAPIErrorUnwrapsToProviderError(t *testing.T) {
	underlying := errors.New("sdk exploded")
	wrapped := fmt.Errorf("calling api: %w", &APIError{
		Provider:   "test",
		StatusCode: 500,
		Err:        underlying,
	})
	if !errors.Is(wrapped, underlying) {
		t.Error("errors.Is does not reach the underlying provider error")
	}
	var apiErr *APIError
	if !errors.As(wrapped, &apiErr) || apiErr.StatusCode != 500 {
		t.Errorf("errors.As did not recover the APIError from %v", wrapped)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  *APIError
		want []string
	}{
		{
			"explicit message",
			&APIError{Provider: "ollama", StatusCode: 503, Message: "model loading"},
			[]string{"ollama", "503", "model loading"},
		},
		{
			"falls back to wrapped error",
			&APIError{Provider: "anthropic", StatusCode: 429, Err: errors.New("rate_limit_error")},
			[]string{"anthropic", "429", "rate_limit_error"},
		},
		{
			"no message at all",
			&APIError{Provider: "anthropic", StatusCode: 500},
			[]string{"anthropic", "500"},
		},
		{
			"no provider named",
			&APIError{StatusCode: 500},
			[]string{"llm", "500"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("Error() = %q, want it to mention %q", got, want)
				}
			}
		})
	}
}
