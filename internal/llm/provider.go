// Package llm defines the provider-agnostic interface bloodhound uses to talk
// to language models. The Anthropic implementation is first-class; others plug
// in behind the same interface. See specs/001-build-spec.md §3.3.
package llm

import "context"

// Role identifies the author of a message in a conversation.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn in a model conversation.
type Message struct {
	Role    Role
	Content string
	// ToolCalls / ToolResults will be added in M1 when the tool-use loop lands.
}

// Request is a single model invocation.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	MaxTokens int
}

// Usage reports token consumption for one invocation.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Cost is a computed dollar cost for a Usage.
type Cost struct {
	USD float64
}

// Response is the model's reply to a Request.
type Response struct {
	Message Message
	Usage   Usage
	// StopReason and tool-call surface land in M1.
}

// Provider runs model turns. Implementations must be safe for concurrent use.
type Provider interface {
	// Complete runs one model turn.
	Complete(ctx context.Context, req Request) (Response, error)
	// CountCost converts usage into dollars for this provider's pricing.
	CountCost(usage Usage) Cost
	// Name identifies the provider (e.g. "anthropic", "ollama").
	Name() string
}
