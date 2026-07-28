# internal/intake

Intake lands with M1 (orchestrator v0: intake → one hound → report).

Per spec §3.1: an HTTP server receiving Alertmanager webhooks, plus a CLI
path (`bloodhound hunt --alert alert.json`) for manual/replay runs.
Normalizes alerts into a `Case` (ID, alert labels, firing time, cluster
context) and dedupes by alert fingerprint within a window.
