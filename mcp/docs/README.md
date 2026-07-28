# mcp-docs

MCP server exposing runbooks and prior-incident notes to bloodhound's
runbook-hound. Lands in M2.

Planned tools: `search_runbooks`, `get_runbook`, `similar_incidents`.
v1 backend: a local markdown directory indexed with SQLite FTS5 — keyword
search is honest and debuggable. Embeddings are a v2 upgrade that must earn
its place with a benchmark delta.
