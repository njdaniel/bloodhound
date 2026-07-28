# mcp-k8s

Read-only MCP server exposing Kubernetes cluster state to bloodhound's
changes-hound. Lands in M2.

Planned tools: `recent_events`, `rollout_history`, `pod_status`,
`node_pressure`, `recent_restarts`. Uses `client-go` under a read-only
ServiceAccount; the RBAC manifest ships in `deploy/`.
