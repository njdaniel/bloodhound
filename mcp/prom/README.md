# mcp-prom

MCP server exposing Prometheus to bloodhound's metrics-hound (and any MCP
client). Built on the official `modelcontextprotocol/go-sdk`. Lands in M1.

Planned tools: `query_range`, `query_instant`, `list_alerts`, `series_metadata`.

Design rule: tools return bounded, model-shaped data — downsampled series,
capped result sets, explicit truncation markers. Tool descriptions are prompt
engineering; write them for a model, not a human.
