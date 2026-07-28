// Command fakeserver is a minimal MCP server used by mcpclient's tests. It
// exposes three tools over stdio:
//
//	echo   — returns "echo: <text>" (happy path, capture golden)
//	fail   — returns a tool error carrying <message> (tool-error-as-value)
//	sleep  — blocks for <ms> milliseconds (timeout and kill-mid-call paths)
//
// It lives under testdata/ so the main module never builds or ships it; the
// tests compile it on demand.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// echoInput is the input payload for the echo tool.
type echoInput struct {
	Text string `json:"text"`
}

// failInput is the input payload for the fail tool.
type failInput struct {
	Message string `json:"message"`
}

// sleepInput is the input payload for the sleep tool.
type sleepInput struct {
	Ms int `json:"ms"`
}

func main() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fakeserver", Version: "0.0.1"}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "echo",
		Description: "Echo the given text back.",
		InputSchema: json.RawMessage(`{"type":"object","required":["text"],"additionalProperties":false,"properties":{"text":{"type":"string"}}}`),
	}, handleEcho)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fail",
		Description: "Always fail with the given message as a tool error.",
		InputSchema: json.RawMessage(`{"type":"object","required":["message"],"additionalProperties":false,"properties":{"message":{"type":"string"}}}`),
	}, handleFail)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sleep",
		Description: "Sleep for the given number of milliseconds.",
		InputSchema: json.RawMessage(`{"type":"object","required":["ms"],"additionalProperties":false,"properties":{"ms":{"type":"integer","minimum":0}}}`),
	}, handleSleep)

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fakeserver:", err)
		os.Exit(1)
	}
}

// handleEcho implements the echo tool.
func handleEcho(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}},
	}, nil, nil
}

// handleFail implements the fail tool. Returning an error makes the SDK
// produce an IsError tool result, not a protocol failure.
func handleFail(_ context.Context, _ *mcp.CallToolRequest, in failInput) (*mcp.CallToolResult, any, error) {
	return nil, nil, fmt.Errorf("tool failed: %s", in.Message)
}

// handleSleep implements the sleep tool.
func handleSleep(ctx context.Context, _ *mcp.CallToolRequest, in sleepInput) (*mcp.CallToolResult, any, error) {
	select {
	case <-time.After(time.Duration(in.Ms) * time.Millisecond):
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "slept"}},
		}, nil, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}
