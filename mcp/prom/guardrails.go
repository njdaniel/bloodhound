package main

import "time"

// Guardrail constants, per spec 002 §2.1. One table, shared by every tool,
// defined only here. Every truncation these caps trigger is marked in the
// tool payload — the model must never mistake a capped result for a
// complete one.
const (
	// QueryTimeout is the default context deadline applied to every
	// upstream Prometheus HTTP call. Overridable via PROM_TIMEOUT.
	QueryTimeout = 15 * time.Second

	// MaxRange caps query_range end−start. Violations are an error, not a
	// clamp — the model is told to narrow the window.
	MaxRange = 24 * time.Hour

	// MaxPointsPerSeries drives step clamping (spec 002 §2.3 step 2).
	MaxPointsPerSeries = 120

	// MaxSeries caps the number of series returned by query_range.
	MaxSeries = 15

	// MaxInstantSamples caps the number of samples returned by query_instant.
	MaxInstantSamples = 100

	// MaxAlerts caps the number of alerts returned by list_alerts.
	MaxAlerts = 50

	// MaxMetadataMetrics caps the number of metrics returned by series_metadata.
	MaxMetadataMetrics = 25

	// MaxLabelValues caps sample values per label key in series_metadata.
	MaxLabelValues = 10

	// MetadataLookback is the discovery window series_metadata sends as
	// start/end on /api/v1/series. Without it Prometheus scans the whole
	// retention period to answer a question whose result is then reduced to
	// MaxMetadataMetrics anyway. The cost of the window is that a metric with
	// no samples in the last hour is invisible to discovery, which the tool
	// description states so the model does not read absence as non-existence.
	MetadataLookback = time.Hour

	// MaxUpstreamSeries is the limit series_metadata sends to
	// /api/v1/series. Recent Prometheus versions honour it; older ones ignore
	// unknown parameters, so the client-side caps stay authoritative and the
	// output is bounded either way. It is far above MaxMetadataMetrics on
	// purpose: one metric contributes many series, and label values are
	// derived from them.
	MaxUpstreamSeries = 2000

	// MaxUpstreamMetadata is the limit series_metadata sends to
	// /api/v1/metadata, which otherwise returns help and type text for every
	// metric on the server. Prometheus truncates in unspecified order, so the
	// value is generous: a metric dropped upstream still appears in the
	// result, just without its type and help.
	MaxUpstreamMetadata = 2000

	// MaxStringLen caps any label value / metadata string, in bytes.
	// Longer strings are truncated to 117 bytes plus "…" (3 bytes).
	MaxStringLen = 120

	// MaxAnnotationLen caps alert annotation values, in bytes, using the
	// same truncate-plus-ellipsis rule as MaxStringLen.
	MaxAnnotationLen = 200

	// MaxResponseBytes caps the serialized tool result; exceeding it
	// triggers point-thinning (spec 002 §2.3 step 6).
	MaxResponseBytes = 32 * 1024

	// FloatFormat renders every sample value. Values ship as strings to
	// dodge float JSON round-trip noise in golden tests.
	FloatFormat = "%.6g"
)

// truncateString enforces a byte cap on s: strings at or under max are
// returned unchanged; longer strings are cut to max−3 bytes (backing up to a
// rune boundary so no partial UTF-8 sequence survives) with "…" appended.
func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len("…")
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && s[cut]&0xC0 == 0x80 { // do not split a UTF-8 sequence
		cut--
	}
	return s[:cut] + "…"
}

// truncateLabels returns a copy of labels with every value capped at
// MaxStringLen. Keys are assumed to be short Prometheus label names.
func truncateLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = truncateString(v, MaxStringLen)
	}
	return out
}
