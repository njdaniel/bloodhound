# mcp-loki

MCP server exposing Loki log search to bloodhound's logs-hound. Lands in M2.

Planned tools: `query_logs` (LogQL), `label_values`, `tail_context` (log window
around a timestamp). Guardrails: hard line caps, mandatory time bounds.
