# internal/intake

Normalizes Alertmanager alerts into the facts a case starts from.

**M1 (shipped):** `ParseFile` / `Parse` read one Alertmanager-format alert JSON
— either a webhook payload (`{"alerts": [...]}`, first firing alert wins) or a
single bare alert object — and normalize it into an `Alert`: name, labels,
annotations, firing time in UTC, fingerprint. `bloodhound hunt --alert
alert.json` is the only caller; the orchestrator's intake phase maps the result
onto its `Case` and assigns the case ID (spec 002 §4.1).

The package holds no orchestrator types on purpose: it parses, and the
orchestrator maps. That keeps the dependency edge one-way.

**M2:** the HTTP server receiving Alertmanager webhooks (spec 001 §3.1), and
dedupe by alert fingerprint within a window — `Alert.Fingerprint` is already
carried through for it.
