package main

import (
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverVersion is reported to MCP clients during initialization.
const serverVersion = "0.1.0"

// Input schemas, exactly as spec 002 §2.2 defines them. They are handed to
// the SDK as raw JSON (validated against the 2020-12 draft) so the wire
// schemas match the spec byte for byte, independent of Go struct inference.
const (
	// queryRangeSchema is the input schema for the query_range tool.
	queryRangeSchema = `{
  "type": "object",
  "required": ["query", "start", "end"],
  "additionalProperties": false,
  "properties": {
    "query": { "type": "string", "description": "PromQL expression" },
    "start": { "type": "string", "format": "date-time", "description": "RFC 3339" },
    "end":   { "type": "string", "format": "date-time", "description": "RFC 3339" },
    "step_seconds": { "type": "integer", "minimum": 1,
      "description": "Requested resolution. The server clamps so a series has at most 120 points; the effective step is echoed back." }
  }
}`

	// queryInstantSchema is the input schema for the query_instant tool.
	queryInstantSchema = `{
  "type": "object",
  "required": ["query"],
  "additionalProperties": false,
  "properties": {
    "query": { "type": "string", "description": "PromQL expression" },
    "time": { "type": "string", "format": "date-time", "description": "RFC 3339 evaluation instant; defaults to now" }
  }
}`

	// listAlertsSchema is the input schema for the list_alerts tool.
	listAlertsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "state": { "type": "string", "enum": ["firing", "pending", "all"],
      "description": "Alert state filter; defaults to \"firing\"" }
  }
}`

	// seriesMetadataSchema is the input schema for the series_metadata tool.
	seriesMetadataSchema = `{
  "type": "object",
  "required": ["match"],
  "additionalProperties": false,
  "properties": {
    "match": { "type": "string",
      "description": "Series selector, e.g. '{namespace=\"shop\"}', or a bare metric name" }
  }
}`
)

// Tool descriptions are prompt engineering: written for a model, with the
// guardrails it must plan around stated up front.
const (
	queryRangeDescription = "Query Prometheus over a time range with a PromQL expression. " +
		"The range (end - start) is limited to 24h — wider windows are an error; narrow your window and retry. " +
		"Resolution is clamped to at most 120 points per series (resolved_step_seconds echoes the effective step) " +
		"and at most 15 series are returned, ranked by (max-min) volatility descending. " +
		"Each series carries stats (min/max/avg/last) computed from full-resolution data — read those before the points. " +
		"Every cap applied is recorded in the truncation block; if series were dropped, narrow the selector and query again. " +
		"Prefer rate()/ratio forms and narrow selectors over broad matches."

	queryInstantDescription = "Evaluate a PromQL expression at a single instant (time defaults to now). " +
		"Returns at most 100 samples, ranked by |value| descending so outliers come first. " +
		"The truncation block records dropped samples; narrow the selector to see specific series. " +
		"Use query_range instead when you need the shape of a value over time."

	listAlertsDescription = "List alerts currently active in Prometheus. " +
		"state filters to \"firing\" (default), \"pending\", or \"all\". " +
		"Alerts are sorted by active_at descending (newest first) and capped at 50; " +
		"annotation values are truncated to 200 characters. " +
		"The truncation block records any dropped alerts."

	seriesMetadataDescription = "Discover metrics and their label structure before writing PromQL. " +
		"match is a series selector (e.g. '{namespace=\"shop\"}') or a bare metric name. " +
		"Returns at most 25 metrics, alphabetical, each with type, help text, and up to 10 sample values per label key. " +
		"The truncation block records dropped metrics; narrow the match selector to see more."
)

// newServer builds the mcp-prom MCP server with the four tools registered.
func newServer(prom *promClient) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcp-prom", Version: serverVersion}, nil)
	ts := &toolServer{prom: prom}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "query_range",
		Description: queryRangeDescription,
		InputSchema: json.RawMessage(queryRangeSchema),
	}, ts.handleQueryRange)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "query_instant",
		Description: queryInstantDescription,
		InputSchema: json.RawMessage(queryInstantSchema),
	}, ts.handleQueryInstant)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_alerts",
		Description: listAlertsDescription,
		InputSchema: json.RawMessage(listAlertsSchema),
	}, ts.handleListAlerts)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "series_metadata",
		Description: seriesMetadataDescription,
		InputSchema: json.RawMessage(seriesMetadataSchema),
	}, ts.handleSeriesMetadata)

	return srv
}
