// Package anthropic implements llm.Provider on top of the official
// github.com/anthropics/anthropic-sdk-go client. Model name and pricing come
// from Config, not from call sites. The SDK's built-in retries are disabled so
// that retry policy lives in exactly one place: the middleware package. API
// failures are translated into llm.APIError here, at the boundary, so that
// retry policy stays provider-agnostic.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/njdaniel/bloodhound/internal/llm"
)

// providerName is this provider's llm.Provider name, also stamped onto the
// llm.APIError values it produces.
const providerName = "anthropic"

// Config selects the model and its pricing. Prices are dollars per million
// tokens, matching how Anthropic publishes them.
type Config struct {
	// Model is the default model ID (e.g. "claude-sonnet-4-6") used when a
	// Request does not name one.
	Model string
	// InputUSDPerMTok is the input-token price in $/MTok.
	InputUSDPerMTok float64
	// OutputUSDPerMTok is the output-token price in $/MTok.
	OutputUSDPerMTok float64
}

// Provider is the Anthropic implementation of llm.Provider. It is safe for
// concurrent use.
type Provider struct {
	cfg    Config
	client sdk.Client
}

// New builds a Provider from cfg. Extra request options (API key, base URL for
// tests, timeouts) are passed through to the SDK client; by default the API
// key comes from ANTHROPIC_API_KEY.
//
// Disabling SDK-level retries is not one of the pass-through options: it is
// applied last, so it wins over any option.WithMaxRetries a caller supplies.
// Retry policy lives in exactly one place — the middleware package — and a
// caller re-enabling SDK retries would silently hide attempts from the retry,
// accounting and capture layers. This is deliberate and not an escape hatch:
// changing the retry behaviour means changing the middleware, not this
// constructor's arguments.
func New(cfg Config, opts ...option.RequestOption) *Provider {
	opts = append(append([]option.RequestOption{}, opts...), option.WithMaxRetries(0))
	return &Provider{cfg: cfg, client: sdk.NewClient(opts...)}
}

// Name identifies this provider.
func (p *Provider) Name() string { return providerName }

// CountCost prices usage with the per-model rates from Config.
func (p *Provider) CountCost(u llm.Usage) llm.Cost {
	return llm.Cost{
		USD: float64(u.InputTokens)*p.cfg.InputUSDPerMTok/1e6 +
			float64(u.OutputTokens)*p.cfg.OutputUSDPerMTok/1e6,
	}
}

// Complete runs one model turn against the Anthropic Messages API.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	params, err := p.buildParams(req)
	if err != nil {
		return llm.Response{}, fmt.Errorf("building anthropic request: %w", err)
	}
	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return llm.Response{}, fmt.Errorf("calling anthropic messages API: %w", translateError(err))
	}
	return translateResponse(msg), nil
}

// translateError converts an SDK API error into the provider-agnostic
// llm.APIError, so retry policy can classify Anthropic failures without
// importing the SDK. This is the one place the translation happens: the SDK
// type is already here, and nothing downstream has to know it exists.
//
// Errors the SDK does not wrap — dial failures, request timeouts, context
// errors — are returned unchanged. They already carry the interfaces
// (net.Error, context) that classification uses, and re-labelling them as API
// errors would invent a status code that never came off the wire.
func translateError(err error) error {
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	return &llm.APIError{
		Provider:   providerName,
		StatusCode: apiErr.StatusCode,
		Err:        err,
	}
}

// buildParams translates a provider-agnostic request into SDK params.
func (p *Provider) buildParams(req llm.Request) (sdk.MessageNewParams, error) {
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}
	params := sdk.MessageNewParams{
		Model:     model,
		MaxTokens: int64(req.MaxTokens),
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System}}
	}

	for i, m := range req.Messages {
		blocks, err := translateBlocks(m.Content)
		if err != nil {
			return sdk.MessageNewParams{}, fmt.Errorf("translating message %d: %w", i, err)
		}
		switch m.Role {
		case llm.RoleUser:
			params.Messages = append(params.Messages, sdk.NewUserMessage(blocks...))
		case llm.RoleAssistant:
			params.Messages = append(params.Messages, sdk.NewAssistantMessage(blocks...))
		default:
			return sdk.MessageNewParams{}, fmt.Errorf("translating message %d: unknown role %q", i, m.Role)
		}
	}

	for _, t := range req.Tools {
		schema, err := translateSchema(t.InputSchema)
		if err != nil {
			return sdk.MessageNewParams{}, fmt.Errorf("translating schema for tool %q: %w", t.Name, err)
		}
		params.Tools = append(params.Tools, sdk.ToolUnionParam{OfTool: &sdk.ToolParam{
			Name:        t.Name,
			Description: sdk.String(t.Description),
			InputSchema: schema,
		}})
	}

	choice, err := translateToolChoice(req.ToolChoice)
	if err != nil {
		return sdk.MessageNewParams{}, err
	}
	if choice != nil {
		params.ToolChoice = *choice
	}
	return params, nil
}

// translateBlocks converts content blocks to SDK content block params.
func translateBlocks(blocks []llm.ContentBlock) ([]sdk.ContentBlockParamUnion, error) {
	out := make([]sdk.ContentBlockParamUnion, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case llm.BlockText:
			out = append(out, sdk.NewTextBlock(b.Text))
		case llm.BlockToolUse:
			if b.ToolUse == nil {
				return nil, fmt.Errorf("tool_use block has nil ToolUse")
			}
			out = append(out, sdk.NewToolUseBlock(b.ToolUse.ID, b.ToolUse.Input, b.ToolUse.Name))
		case llm.BlockToolResult:
			if b.ToolResult == nil {
				return nil, fmt.Errorf("tool_result block has nil ToolResult")
			}
			out = append(out, sdk.NewToolResultBlock(b.ToolResult.ToolUseID, b.ToolResult.Content, b.ToolResult.IsError))
		default:
			return nil, fmt.Errorf("unknown content block type %q", b.Type)
		}
	}
	return out, nil
}

// translateSchema converts a raw JSON Schema document into the SDK's input
// schema param, preserving keys beyond properties/required via ExtraFields.
func translateSchema(raw json.RawMessage) (sdk.ToolInputSchemaParam, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return sdk.ToolInputSchemaParam{}, fmt.Errorf("parsing input schema: %w", err)
	}
	var schema sdk.ToolInputSchemaParam
	extras := make(map[string]any)
	for k, v := range doc {
		switch k {
		case "type":
			// The SDK marshals type "object" itself.
		case "properties":
			schema.Properties = v
		case "required":
			items, ok := v.([]any)
			if !ok {
				return sdk.ToolInputSchemaParam{}, fmt.Errorf("parsing input schema: required is not an array")
			}
			for _, item := range items {
				s, ok := item.(string)
				if !ok {
					return sdk.ToolInputSchemaParam{}, fmt.Errorf("parsing input schema: required entry is not a string")
				}
				schema.Required = append(schema.Required, s)
			}
		default:
			extras[k] = v
		}
	}
	if len(extras) > 0 {
		schema.ExtraFields = extras
	}
	return schema, nil
}

// translateToolChoice maps the provider-agnostic tool choice onto the SDK
// union. A nil result means the field is omitted (provider default: auto).
func translateToolChoice(tc llm.ToolChoice) (*sdk.ToolChoiceUnionParam, error) {
	switch tc.Type {
	case "", llm.ToolChoiceAuto:
		return nil, nil
	case llm.ToolChoiceRequired:
		return &sdk.ToolChoiceUnionParam{OfAny: &sdk.ToolChoiceAnyParam{}}, nil
	case llm.ToolChoiceTool:
		if tc.Name == "" {
			return nil, fmt.Errorf("tool choice %q requires a tool name", tc.Type)
		}
		return &sdk.ToolChoiceUnionParam{OfTool: &sdk.ToolChoiceToolParam{Name: tc.Name}}, nil
	default:
		return nil, fmt.Errorf("unknown tool choice type %q", tc.Type)
	}
}

// translateResponse converts an SDK message into the provider-agnostic
// response. Block types the surface does not model (e.g. thinking) are
// dropped; the stop reason passes through verbatim.
func translateResponse(msg *sdk.Message) llm.Response {
	out := llm.Message{Role: llm.RoleAssistant}
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			out.Content = append(out.Content, llm.TextBlock(b.Text))
		case sdk.ToolUseBlock:
			out.Content = append(out.Content, llm.ToolUseBlock(llm.ToolUse{
				ID:    b.ID,
				Name:  b.Name,
				Input: b.Input,
			}))
		}
	}
	return llm.Response{
		Message:    out,
		StopReason: llm.StopReason(msg.StopReason),
		Usage: llm.Usage{
			InputTokens:  int(msg.Usage.InputTokens),
			OutputTokens: int(msg.Usage.OutputTokens),
		},
	}
}
