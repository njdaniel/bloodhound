package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// wantSchemas maps every tool name to its spec 002 §2.2 input schema.
var wantSchemas = map[string]string{
	"query_range":     queryRangeSchema,
	"query_instant":   queryInstantSchema,
	"list_alerts":     listAlertsSchema,
	"series_metadata": seriesMetadataSchema,
}

// buildServerBinary compiles the mcp-prom binary into a temp dir.
func buildServerBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mcp-prom")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building mcp-prom binary: %v\n%s", err, out)
	}
	return bin
}

// TestStdioConformance spawns the built binary over stdio with the go-sdk
// client, lists the tools, asserts names and input schemas, and calls each
// tool once against the fake Prometheus.
func TestStdioConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary spawn in -short mode")
	}
	fake := goldenFake(t)
	bin := buildServerBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "PROM_URL="+fake.url(), "PROM_TIMEOUT=5s")
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-prom-conformance", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting to mcp-prom over stdio: %v", err)
	}
	defer session.Close()

	// Tool listing: exactly the four tools, with the spec schemas.
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("listing tools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	if len(byName) != len(wantSchemas) {
		t.Errorf("server lists %d tools, want %d", len(byName), len(wantSchemas))
	}
	for name, wantRaw := range wantSchemas {
		tool, ok := byName[name]
		if !ok {
			t.Errorf("tool %q not listed", name)
			continue
		}
		if tool.Description == "" {
			t.Errorf("tool %q has no description", name)
		}
		var want, got any
		if err := json.Unmarshal([]byte(wantRaw), &want); err != nil {
			t.Fatalf("parsing expected schema for %q: %v", name, err)
		}
		gotRaw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("re-marshaling listed schema for %q: %v", name, err)
		}
		if err := json.Unmarshal(gotRaw, &got); err != nil {
			t.Fatalf("parsing listed schema for %q: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("tool %q input schema differs from spec:\n got: %s\nwant: %s", name, gotRaw, wantRaw)
		}
	}

	// Call each tool once end-to-end; every result must be one text content
	// block containing valid JSON with a truncation block.
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"query_range", map[string]any{
			"query": "up", "start": "2026-07-28T10:00:00Z", "end": "2026-07-28T11:00:00Z", "step_seconds": 60,
		}},
		{"query_instant", map[string]any{"query": "up", "time": "2026-07-28T10:15:00Z"}},
		{"list_alerts", map[string]any{"state": "firing"}},
		{"series_metadata", map[string]any{"match": `{namespace="shop"}`}},
	}
	for _, call := range calls {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.tool, Arguments: call.args})
		if err != nil {
			t.Fatalf("calling %s: %v", call.tool, err)
		}
		if res.IsError {
			t.Fatalf("calling %s returned tool error: %s", call.tool, resultText(t, res))
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
			t.Fatalf("%s result is not valid JSON: %v", call.tool, err)
		}
		if _, ok := payload["truncation"]; !ok {
			t.Errorf("%s result has no truncation block", call.tool)
		}
	}

	// A too-wide window must come back as a tool error the model can act on,
	// not a protocol failure.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "query_range", Arguments: map[string]any{
		"query": "up", "start": "2026-07-27T00:00:00Z", "end": "2026-07-28T12:00:00Z",
	}})
	if err != nil {
		t.Fatalf("wide-window call became a protocol failure: %v", err)
	}
	if !res.IsError {
		t.Fatal("wide-window call did not return a tool error")
	}
}
