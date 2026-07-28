// Package llm defines the provider-agnostic interface bloodhound uses to talk
// to language models, including the tool-use surface: tool definitions on the
// request, tool_use blocks on the response, and tool_result blocks fed back on
// the next turn. The Anthropic implementation is first-class; others plug in
// behind the same interface. See specs/001-build-spec.md §3.3 and
// specs/002-m1-metrics-path.md §3.1.
package llm

import (
	"context"
	"encoding/json"
)

// Role identifies the author of a message in a conversation.
type Role string

// The two conversation roles. Tool results are carried inside user messages,
// matching the wire shape of the Anthropic Messages API.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType discriminates the kind of a ContentBlock.
type BlockType string

// Content block kinds. A text block carries prose, a tool_use block carries a
// model-initiated tool invocation, and a tool_result block carries the outcome
// of executing one back to the model.
const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// ContentBlock is one unit of message content. Exactly one of Text, ToolUse,
// or ToolResult is meaningful, selected by Type.
type ContentBlock struct {
	Type       BlockType   `json:"type"`
	Text       string      `json:"text,omitempty"`
	ToolUse    *ToolUse    `json:"tool_use,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

// TextBlock builds a text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: BlockText, Text: text}
}

// ToolUseBlock builds a tool_use content block.
func ToolUseBlock(tu ToolUse) ContentBlock {
	return ContentBlock{Type: BlockToolUse, ToolUse: &tu}
}

// ToolResultBlock builds a tool_result content block answering the tool_use
// identified by toolUseID. Set isError when the tool failed and content
// carries an error message the model should react to.
func ToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return ContentBlock{Type: BlockToolResult, ToolResult: &ToolResult{
		ToolUseID: toolUseID,
		Content:   content,
		IsError:   isError,
	}}
}

// Tool describes one tool the model may call. InputSchema is a JSON Schema
// document for the tool's input object; it is passed to the provider verbatim.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolUse is a model request to invoke a tool. Input is the raw JSON argument
// object; callers unmarshal it against the tool's schema.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult carries the outcome of executing a ToolUse back to the model.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// StopReason reports why the model ended its turn.
type StopReason string

// Stop reasons on the provider-agnostic surface (spec 002 §3.1). Providers may
// pass through additional values verbatim; callers should treat anything
// unrecognized as terminal.
const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
)

// ToolChoiceType discriminates how the model is allowed to choose tools.
type ToolChoiceType string

// Tool choice modes: auto lets the model decide (the default), required
// forces at least one tool call, and tool forces a call to one specific tool.
const (
	ToolChoiceAuto     ToolChoiceType = "auto"
	ToolChoiceRequired ToolChoiceType = "required"
	ToolChoiceTool     ToolChoiceType = "tool"
)

// ToolChoice constrains the model's tool selection for one request. The zero
// value means auto. Name is only meaningful when Type is ToolChoiceTool.
type ToolChoice struct {
	Type ToolChoiceType `json:"type,omitempty"`
	Name string         `json:"name,omitempty"`
}

// ChooseTool returns a ToolChoice that forces the model to call the named
// tool. Used to force finding submission (spec 002 §3.2).
func ChooseTool(name string) ToolChoice {
	return ToolChoice{Type: ToolChoiceTool, Name: name}
}

// Message is one turn in a model conversation. Content is an ordered list of
// blocks (text, tool_use, tool_result) rather than a bare string.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// UserMessage builds a user-role message from content blocks.
func UserMessage(blocks ...ContentBlock) Message {
	return Message{Role: RoleUser, Content: blocks}
}

// AssistantMessage builds an assistant-role message from content blocks.
func AssistantMessage(blocks ...ContentBlock) Message {
	return Message{Role: RoleAssistant, Content: blocks}
}

// Text concatenates the message's text blocks, ignoring tool blocks.
func (m Message) Text() string {
	var out string
	for _, b := range m.Content {
		if b.Type == BlockText {
			out += b.Text
		}
	}
	return out
}

// ToolUses returns the tool_use blocks of the message, in order.
func (m Message) ToolUses() []ToolUse {
	var uses []ToolUse
	for _, b := range m.Content {
		if b.Type == BlockToolUse && b.ToolUse != nil {
			uses = append(uses, *b.ToolUse)
		}
	}
	return uses
}

// Request is a single model invocation.
type Request struct {
	Model      string     `json:"model"`
	System     string     `json:"system,omitempty"`
	Messages   []Message  `json:"messages"`
	MaxTokens  int        `json:"max_tokens"`
	Tools      []Tool     `json:"tools,omitempty"`
	ToolChoice ToolChoice `json:"tool_choice,omitzero"`
}

// Usage reports token consumption for one invocation.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Add returns the element-wise sum of two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + o.InputTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
	}
}

// Cost is a computed dollar cost for a Usage.
type Cost struct {
	USD float64 `json:"usd"`
}

// Response is the model's reply to a Request.
type Response struct {
	Message    Message    `json:"message"`
	StopReason StopReason `json:"stop_reason"`
	Usage      Usage      `json:"usage"`
}

// Provider runs model turns. Implementations must be safe for concurrent use.
type Provider interface {
	// Complete runs one model turn. Tool use is expressed via the request's
	// tool definitions; the response either finishes or requests tool calls.
	Complete(ctx context.Context, req Request) (Response, error)
	// CountCost converts usage into dollars for this provider's pricing.
	CountCost(usage Usage) Cost
	// Name identifies the provider (e.g. "anthropic", "ollama").
	Name() string
}
