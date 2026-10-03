package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- query_instant ---------------------------------------------------------

// gotInstant mirrors the query_instant output payload for assertions.
type gotInstant struct {
	ResultType string `json:"result_type"`
	Samples    []struct {
		Labels    map[string]string `json:"labels"`
		Value     string            `json:"value"`
		Timestamp int64             `json:"timestamp"`
	} `json:"samples"`
	Truncation struct {
		SamplesTotal    int    `json:"samples_total"`
		SamplesReturned int    `json:"samples_returned"`
		Note            string `json:"note"`
	} `json:"truncation"`
}

func decodeInstant(t *testing.T, res *mcp.CallToolResult) gotInstant {
	t.Helper()
	var g gotInstant
	if err := json.Unmarshal([]byte(resultText(t, res)), &g); err != nil {
		t.Fatalf("decoding query_instant payload: %v", err)
	}
	return g
}

// vectorJSON builds a canned /api/v1/query vector success body.
func vectorJSON(t *testing.T, samples []map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": samples},
	})
	if err != nil {
		t.Fatalf("building vector fixture: %v", err)
	}
	return string(body)
}

// sampleFixture builds one vector sample map for vectorJSON.
func sampleFixture(labels map[string]string, ts any, value string) map[string]any {
	return map[string]any{"metric": labels, "value": []any{ts, value}}
}

func TestQueryInstantRankingByAbsValueWithLabelsetTieBreak(t *testing.T) {
	fake := newFakeProm(t)
	fake.set("/api/v1/query", 200, vectorJSON(t, []map[string]any{
		sampleFixture(map[string]string{"pod": "small"}, 1753700000, "3"),
		sampleFixture(map[string]string{"pod": "z-tied"}, 1753700000, "5"),
		sampleFixture(map[string]string{"pod": "negative"}, 1753700000, "-10"),
		sampleFixture(map[string]string{"pod": "a-tied"}, 1753700000, "5"),
	}))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleQueryInstant(context.Background(), nil, queryInstantInput{
		Query: "up", Time: "2026-07-28T10:00:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryInstant: %v", err)
	}
	g := decodeInstant(t, res)
	var order []string
	for _, s := range g.Samples {
		order = append(order, s.Labels["pod"])
	}
	// |−10| first, then the two tied 5s in labelset order, then 3.
	want := []string{"negative", "a-tied", "z-tied", "small"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("sample order = %v, want %v", order, want)
	}
	if g.Samples[0].Value != "-10" {
		t.Errorf("top sample value = %q, want \"-10\"", g.Samples[0].Value)
	}
}

func TestQueryInstantSampleCap(t *testing.T) {
	fake := newFakeProm(t)
	var samples []map[string]any
	for i := 0; i < MaxInstantSamples+5; i++ {
		samples = append(samples, sampleFixture(
			map[string]string{"pod": fmt.Sprintf("pod-%03d", i)}, 1753700000, fmt.Sprintf("%d", i)))
	}
	fake.set("/api/v1/query", 200, vectorJSON(t, samples))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleQueryInstant(context.Background(), nil, queryInstantInput{Query: "up"})
	if err != nil {
		t.Fatalf("handleQueryInstant: %v", err)
	}
	g := decodeInstant(t, res)
	if g.Truncation.SamplesTotal != MaxInstantSamples+5 || g.Truncation.SamplesReturned != MaxInstantSamples {
		t.Errorf("truncation counts = %d/%d, want %d/%d",
			g.Truncation.SamplesReturned, g.Truncation.SamplesTotal, MaxInstantSamples, MaxInstantSamples+5)
	}
	if len(g.Samples) != MaxInstantSamples {
		t.Fatalf("got %d samples, want %d", len(g.Samples), MaxInstantSamples)
	}
	if !strings.Contains(g.Truncation.Note, "5 samples dropped") {
		t.Errorf("truncation note %q missing drop count", g.Truncation.Note)
	}
	// Smallest |value| samples (0..4) are the ones dropped.
	for _, s := range g.Samples {
		if s.Value == "0" || s.Value == "4" {
			t.Errorf("low-|value| sample %q survived the cap", s.Value)
		}
	}
}

func TestQueryInstantScalar(t *testing.T) {
	fake := newFakeProm(t)
	fake.set("/api/v1/query", 200,
		`{"status":"success","data":{"resultType":"scalar","result":[1753700000.123,"3.14159265"]}}`)
	ts := newTestToolServer(fake)

	res, _, err := ts.handleQueryInstant(context.Background(), nil, queryInstantInput{Query: "pi()"})
	if err != nil {
		t.Fatalf("handleQueryInstant: %v", err)
	}
	g := decodeInstant(t, res)
	if g.ResultType != "scalar" {
		t.Errorf("result_type = %q, want scalar", g.ResultType)
	}
	if len(g.Samples) != 1 || g.Samples[0].Value != "3.14159" || g.Samples[0].Timestamp != 1753700000 {
		t.Errorf("scalar sample = %+v, want value 3.14159 (%%.6g) at 1753700000", g.Samples)
	}
}

func TestQueryInstantBadPromQLError(t *testing.T) {
	fake := newFakeProm(t)
	fake.set("/api/v1/query", 400,
		`{"status":"error","errorType":"bad_data","error":"invalid parameter \"query\": parse error"}`)
	ts := newTestToolServer(fake)

	_, _, err := ts.handleQueryInstant(context.Background(), nil, queryInstantInput{Query: "up("})
	if err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Errorf("got %v, want upstream parse error", err)
	}
}

// --- list_alerts -----------------------------------------------------------

// gotAlerts mirrors the list_alerts output payload for assertions.
type gotAlerts struct {
	Alerts []struct {
		Name        string            `json:"name"`
		State       string            `json:"state"`
		ActiveAt    string            `json:"active_at"`
		Value       string            `json:"value"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"alerts"`
	Truncation struct {
		AlertsTotal    int    `json:"alerts_total"`
		AlertsReturned int    `json:"alerts_returned"`
		Note           string `json:"note"`
	} `json:"truncation"`
}

func decodeAlerts(t *testing.T, res *mcp.CallToolResult) gotAlerts {
	t.Helper()
	var g gotAlerts
	if err := json.Unmarshal([]byte(resultText(t, res)), &g); err != nil {
		t.Fatalf("decoding list_alerts payload: %v", err)
	}
	return g
}

// alertsJSON builds a canned /api/v1/alerts success body.
func alertsJSON(t *testing.T, alerts []map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"alerts": alerts},
	})
	if err != nil {
		t.Fatalf("building alerts fixture: %v", err)
	}
	return string(body)
}

// alertFixture builds one alert map for alertsJSON.
func alertFixture(name, state, activeAt string, annotations map[string]string) map[string]any {
	return map[string]any{
		"labels":      map[string]string{"alertname": name, "severity": "page"},
		"annotations": annotations,
		"state":       state,
		"activeAt":    activeAt,
		"value":       "1e+00",
	}
}

func TestListAlertsDefaultsToFiringSortedNewestFirst(t *testing.T) {
	fake := newFakeProm(t)
	fake.set("/api/v1/alerts", 200, alertsJSON(t, []map[string]any{
		alertFixture("OldFiring", "firing", "2026-07-28T08:00:00Z", nil),
		alertFixture("Pending", "pending", "2026-07-28T10:30:00Z", nil),
		alertFixture("NewFiring", "firing", "2026-07-28T10:00:00Z", nil),
	}))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleListAlerts(context.Background(), nil, listAlertsInput{})
	if err != nil {
		t.Fatalf("handleListAlerts: %v", err)
	}
	g := decodeAlerts(t, res)
	if len(g.Alerts) != 2 {
		t.Fatalf("got %d alerts with default state, want 2 firing", len(g.Alerts))
	}
	if g.Alerts[0].Name != "NewFiring" || g.Alerts[1].Name != "OldFiring" {
		t.Errorf("alert order = %s, %s; want NewFiring, OldFiring (active_at desc)",
			g.Alerts[0].Name, g.Alerts[1].Name)
	}
	if g.Alerts[0].Value != "1" {
		t.Errorf("alert value = %q, want \"1\" (%%.6g of 1e+00)", g.Alerts[0].Value)
	}
}

func TestListAlertsStateAllIncludesPending(t *testing.T) {
	fake := newFakeProm(t)
	fake.set("/api/v1/alerts", 200, alertsJSON(t, []map[string]any{
		alertFixture("Firing", "firing", "2026-07-28T10:00:00Z", nil),
		alertFixture("Pending", "pending", "2026-07-28T10:30:00Z", nil),
	}))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleListAlerts(context.Background(), nil, listAlertsInput{State: "all"})
	if err != nil {
		t.Fatalf("handleListAlerts: %v", err)
	}
	if g := decodeAlerts(t, res); len(g.Alerts) != 2 {
		t.Errorf("state=all returned %d alerts, want 2", len(g.Alerts))
	}
}

func TestListAlertsAnnotationTruncationAndCap(t *testing.T) {
	fake := newFakeProm(t)
	longAnnotation := strings.Repeat("x", 250)
	var alerts []map[string]any
	for i := 0; i < MaxAlerts+5; i++ {
		alerts = append(alerts, alertFixture(
			fmt.Sprintf("Alert%02d", i), "firing",
			fmt.Sprintf("2026-07-28T10:%02d:00Z", i%60),
			map[string]string{"description": longAnnotation}))
	}
	fake.set("/api/v1/alerts", 200, alertsJSON(t, alerts))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleListAlerts(context.Background(), nil, listAlertsInput{State: "firing"})
	if err != nil {
		t.Fatalf("handleListAlerts: %v", err)
	}
	g := decodeAlerts(t, res)
	if g.Truncation.AlertsTotal != MaxAlerts+5 || g.Truncation.AlertsReturned != MaxAlerts {
		t.Errorf("truncation counts = %d/%d, want %d/%d",
			g.Truncation.AlertsReturned, g.Truncation.AlertsTotal, MaxAlerts, MaxAlerts+5)
	}
	desc := g.Alerts[0].Annotations["description"]
	if len(desc) != MaxAlertAnnotationLen || !strings.HasSuffix(desc, "…") {
		t.Errorf("annotation length = %d (ellipsis: %v), want %d bytes ending in …",
			len(desc), strings.HasSuffix(desc, "…"), MaxAlertAnnotationLen)
	}
}

// --- series_metadata -------------------------------------------------------

// gotMetadata mirrors the series_metadata output payload for assertions.
type gotMetadata struct {
	Metrics []struct {
		Name   string              `json:"name"`
		Type   string              `json:"type"`
		Help   string              `json:"help"`
		Labels map[string][]string `json:"labels"`
	} `json:"metrics"`
	Truncation struct {
		MetricsTotal    int    `json:"metrics_total"`
		MetricsReturned int    `json:"metrics_returned"`
		Note            string `json:"note"`
	} `json:"truncation"`
}

func decodeMetadata(t *testing.T, res *mcp.CallToolResult) gotMetadata {
	t.Helper()
	var g gotMetadata
	if err := json.Unmarshal([]byte(resultText(t, res)), &g); err != nil {
		t.Fatalf("decoding series_metadata payload: %v", err)
	}
	return g
}

// seriesJSON builds a canned /api/v1/series success body.
func seriesJSON(t *testing.T, sets []map[string]string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"status": "success", "data": sets})
	if err != nil {
		t.Fatalf("building series fixture: %v", err)
	}
	return string(body)
}

// seriesJSONWarned builds a canned /api/v1/series success body carrying the
// warnings Prometheus attaches when a server-side limit truncated the result.
func seriesJSONWarned(t *testing.T, sets []map[string]string, warnings ...string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"status": "success", "data": sets, "warnings": warnings})
	if err != nil {
		t.Fatalf("building series fixture: %v", err)
	}
	return string(body)
}

func TestSeriesMetadataAlphabeticalWithLabelValues(t *testing.T) {
	fake := newFakeProm(t)
	fake.set("/api/v1/series", 200, seriesJSON(t, []map[string]string{
		{"__name__": "http_requests_total", "pod": "b", "namespace": "shop"},
		{"__name__": "http_requests_total", "pod": "a", "namespace": "shop"},
		{"__name__": "cpu_seconds_total", "mode": "idle"},
	}))
	fake.set("/api/v1/metadata", 200, `{"status":"success","data":{
		"http_requests_total":[{"type":"counter","help":"Total HTTP requests.","unit":""}],
		"cpu_seconds_total":[{"type":"counter","help":"CPU time.","unit":""}]}}`)
	ts := newTestToolServer(fake)

	res, _, err := ts.handleSeriesMetadata(context.Background(), nil, seriesMetadataInput{Match: `{__name__=~".+"}`})
	if err != nil {
		t.Fatalf("handleSeriesMetadata: %v", err)
	}
	g := decodeMetadata(t, res)
	if len(g.Metrics) != 2 || g.Metrics[0].Name != "cpu_seconds_total" || g.Metrics[1].Name != "http_requests_total" {
		t.Fatalf("metrics = %+v, want cpu_seconds_total then http_requests_total (alphabetical)", g.Metrics)
	}
	if g.Metrics[1].Type != "counter" || g.Metrics[1].Help != "Total HTTP requests." {
		t.Errorf("http_requests_total metadata = %q/%q, want counter/help text", g.Metrics[1].Type, g.Metrics[1].Help)
	}
	if pods := g.Metrics[1].Labels["pod"]; fmt.Sprint(pods) != fmt.Sprint([]string{"a", "b"}) {
		t.Errorf("pod label values = %v, want [a b] sorted", pods)
	}
}

func TestSeriesMetadataMetricAndLabelValueCaps(t *testing.T) {
	fake := newFakeProm(t)
	var sets []map[string]string
	for i := 0; i < MaxMetadataMetrics+5; i++ {
		sets = append(sets, map[string]string{"__name__": fmt.Sprintf("metric_%02d", i)})
	}
	for i := 0; i < MaxLabelValues+2; i++ {
		sets = append(sets, map[string]string{"__name__": "metric_00", "pod": fmt.Sprintf("pod-%02d", i)})
	}
	fake.set("/api/v1/series", 200, seriesJSON(t, sets))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleSeriesMetadata(context.Background(), nil, seriesMetadataInput{Match: `{__name__=~"metric.*"}`})
	if err != nil {
		t.Fatalf("handleSeriesMetadata: %v", err)
	}
	g := decodeMetadata(t, res)
	if g.Truncation.MetricsTotal != MaxMetadataMetrics+5 || g.Truncation.MetricsReturned != MaxMetadataMetrics {
		t.Errorf("truncation counts = %d/%d, want %d/%d",
			g.Truncation.MetricsReturned, g.Truncation.MetricsTotal, MaxMetadataMetrics, MaxMetadataMetrics+5)
	}
	if !strings.Contains(g.Truncation.Note, "5 metrics dropped") {
		t.Errorf("truncation note %q missing metric drop count", g.Truncation.Note)
	}
	if pods := g.Metrics[0].Labels["pod"]; len(pods) != MaxLabelValues {
		t.Errorf("metric_00 pod values = %d, want capped at %d", len(pods), MaxLabelValues)
	}
}

// TestSeriesMetadataBoundsUpstreamRequests pins the wire request, not the
// payload: an unbounded /api/v1/series makes Prometheus scan its full
// retention and an unbounded /api/v1/metadata describes every metric on the
// server, both to produce a result capped at 25 metrics (issue #11).
func TestSeriesMetadataBoundsUpstreamRequests(t *testing.T) {
	fake := newFakeProm(t)
	ts := newTestToolServer(fake)

	before := time.Now()
	if _, _, err := ts.handleSeriesMetadata(context.Background(), nil, seriesMetadataInput{Match: `{namespace="shop"}`}); err != nil {
		t.Fatalf("handleSeriesMetadata: %v", err)
	}
	after := time.Now()

	series := fake.lastParams("/api/v1/series")
	if got := series.Get("match[]"); got != `{namespace="shop"}` {
		t.Errorf("series match[] = %q, want the selector unchanged", got)
	}
	start, end := parseUnixParam(t, series, "start"), parseUnixParam(t, series, "end")
	if window := end.Sub(start); window != MetadataLookback {
		t.Errorf("series window = %v, want MetadataLookback (%v)", window, MetadataLookback)
	}
	if end.Before(before.Add(-time.Second)) || end.After(after.Add(time.Second)) {
		t.Errorf("series end = %v, want the window to end at call time (%v..%v)", end, before, after)
	}
	if got := series.Get("limit"); got != strconv.Itoa(MaxUpstreamSeries) {
		t.Errorf("series limit = %q, want %d", got, MaxUpstreamSeries)
	}
	if got := fake.lastParams("/api/v1/metadata").Get("limit"); got != strconv.Itoa(MaxUpstreamMetadata) {
		t.Errorf("metadata limit = %q, want %d", got, MaxUpstreamMetadata)
	}
}

// parseUnixParam reads a Prometheus unix-seconds timestamp parameter.
func parseUnixParam(t *testing.T, params url.Values, key string) time.Time {
	t.Helper()
	raw := params.Get(key)
	secs, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("parsing %s=%q as unix seconds: %v", key, raw, err)
	}
	return time.UnixMilli(int64(secs * 1000))
}

// TestSeriesMetadataMarksUpstreamSeriesCap covers a server that honours the
// limit. Prometheus signals that with a warning and nothing else — the result
// itself looks complete — so an unmarked cap is one the model reads as a
// complete answer.
func TestSeriesMetadataMarksUpstreamSeriesCap(t *testing.T) {
	fake := newFakeProm(t)
	fake.set("/api/v1/series", 200, seriesJSONWarned(t, []map[string]string{
		{"__name__": "metric_00", "pod": "a"},
	}, "results truncated due to limit"))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleSeriesMetadata(context.Background(), nil, seriesMetadataInput{Match: `{__name__=~".+"}`})
	if err != nil {
		t.Fatalf("handleSeriesMetadata: %v", err)
	}
	note := decodeMetadata(t, res).Truncation.Note
	if !strings.Contains(note, "truncated the series lookup") {
		t.Errorf("truncation note %q does not report the upstream series cap", note)
	}
	if got := strings.Count(note, "Narrow the match selector."); got != 1 {
		t.Errorf("note %q repeats the narrow-the-selector advice %d times, want once", note, got)
	}
}

// TestSeriesMetadataPassesThroughNonTruncationWarning covers the other reason
// a Prometheus-compatible backend warns: a Thanos/Cortex partial response, or
// a failing remote_read. Nothing was truncated and narrowing the selector will
// not help, so reporting the series limit would hand the model a fabricated
// cause for a real problem. The warning itself is what it can act on.
func TestSeriesMetadataPassesThroughNonTruncationWarning(t *testing.T) {
	const warning = "40 store(s) unavailable; partial response"
	fake := newFakeProm(t)
	fake.set("/api/v1/series", 200, seriesJSONWarned(t, []map[string]string{
		{"__name__": "metric_00", "pod": "a"},
	}, warning))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleSeriesMetadata(context.Background(), nil, seriesMetadataInput{Match: `{__name__=~".+"}`})
	if err != nil {
		t.Fatalf("handleSeriesMetadata: %v", err)
	}
	note := decodeMetadata(t, res).Truncation.Note
	if strings.Contains(note, "truncated the series lookup") {
		t.Errorf("note %q blames the series limit for a warning that is not a truncation", note)
	}
	if strings.Contains(note, "Narrow the match selector.") {
		t.Errorf("note %q advises narrowing the selector, which does not help with %q", note, warning)
	}
	if !strings.Contains(note, warning) {
		t.Errorf("note %q drops the backend's warning %q instead of passing it through", note, warning)
	}
}

// TestSeriesMetadataOldServerIgnoringLimit is the case the guardrails must not
// get wrong: a Prometheus too old to know the limit parameter returns
// everything. The output still has to be bounded by the client-side caps, and
// the result must not claim an upstream truncation that never happened —
// counting series instead of reading warnings gets this exactly backwards.
func TestSeriesMetadataOldServerIgnoringLimit(t *testing.T) {
	fake := newFakeProm(t)
	const (
		metrics       = 100
		seriesPerName = 30 // 3000 series, well past MaxUpstreamSeries
	)
	sets := make([]map[string]string, 0, metrics*seriesPerName)
	for m := 0; m < metrics; m++ {
		for s := 0; s < seriesPerName; s++ {
			sets = append(sets, map[string]string{
				"__name__": fmt.Sprintf("metric_%03d", m),
				"pod":      fmt.Sprintf("pod-%03d", s),
			})
		}
	}
	fake.set("/api/v1/series", 200, seriesJSON(t, sets)) // no warnings: limit ignored
	ts := newTestToolServer(fake)

	res, _, err := ts.handleSeriesMetadata(context.Background(), nil, seriesMetadataInput{Match: `{__name__=~".+"}`})
	if err != nil {
		t.Fatalf("handleSeriesMetadata: %v", err)
	}
	g := decodeMetadata(t, res)
	if len(g.Metrics) != MaxMetadataMetrics || g.Truncation.MetricsReturned != MaxMetadataMetrics {
		t.Errorf("returned %d metrics (truncation says %d), want the client-side cap of %d",
			len(g.Metrics), g.Truncation.MetricsReturned, MaxMetadataMetrics)
	}
	if g.Truncation.MetricsTotal != metrics {
		t.Errorf("metrics_total = %d, want the true %d the server returned", g.Truncation.MetricsTotal, metrics)
	}
	for _, m := range g.Metrics {
		if len(m.Labels["pod"]) > MaxLabelValues {
			t.Errorf("%s has %d pod values, want at most %d", m.Name, len(m.Labels["pod"]), MaxLabelValues)
		}
	}
	if note := g.Truncation.Note; strings.Contains(note, "truncated the series lookup") {
		t.Errorf("note %q claims an upstream truncation, but the server returned every series", note)
	}
}

// TestSeriesMetadataMarksUpstreamMetadataCap covers the metadata limit, which
// Prometheus applies without any warning and in target-iteration order: two
// calls a minute apart can keep different metrics. Unmarked, that turns an
// empty type/help — previously an unambiguous "no metadata registered" — into
// a nondeterministic maybe.
func TestSeriesMetadataMarksUpstreamMetadataCap(t *testing.T) {
	// A metadata map at the limit, optionally containing the one metric the
	// series lookup found.
	fullMetadata := func(t *testing.T, includeWanted bool) string {
		t.Helper()
		entries := map[string]any{}
		for i := 0; i < MaxUpstreamMetadata; i++ {
			entries[fmt.Sprintf("other_metric_%04d", i)] = []map[string]string{{"type": "counter", "help": "h", "unit": ""}}
		}
		if includeWanted {
			entries["metric_00"] = []map[string]string{{"type": "gauge", "help": "Present.", "unit": ""}}
		}
		body, err := json.Marshal(map[string]any{"status": "success", "data": entries})
		if err != nil {
			t.Fatalf("building metadata fixture: %v", err)
		}
		return string(body)
	}

	run := func(t *testing.T, metadataBody string) gotMetadata {
		t.Helper()
		fake := newFakeProm(t)
		fake.set("/api/v1/series", 200, seriesJSON(t, []map[string]string{{"__name__": "metric_00", "pod": "a"}}))
		fake.set("/api/v1/metadata", 200, metadataBody)
		res, _, err := newTestToolServer(fake).handleSeriesMetadata(context.Background(), nil, seriesMetadataInput{Match: `{__name__=~".+"}`})
		if err != nil {
			t.Fatalf("handleSeriesMetadata: %v", err)
		}
		return decodeMetadata(t, res)
	}

	t.Run("empty type and help are marked as possibly unfetched", func(t *testing.T) {
		g := run(t, fullMetadata(t, false))
		if g.Metrics[0].Type != "" {
			t.Fatalf("fixture assumption broken: metric_00 got type %q", g.Metrics[0].Type)
		}
		if note := g.Truncation.Note; !strings.Contains(note, "metadata lookup hit") {
			t.Errorf("truncation note %q does not report the metadata cap that made type/help empty", note)
		}
	})

	// A full map only matters if something in the result is missing metadata
	// because of it; otherwise the note is noise, and it would fire on every
	// server that happens to hold exactly MaxUpstreamMetadata metrics.
	t.Run("complete metadata is not marked", func(t *testing.T) {
		g := run(t, fullMetadata(t, true))
		if g.Metrics[0].Type != "gauge" {
			t.Fatalf("fixture assumption broken: metric_00 got type %q, want gauge", g.Metrics[0].Type)
		}
		if note := g.Truncation.Note; strings.Contains(note, "metadata lookup hit") {
			t.Errorf("truncation note %q reports the metadata cap although every metric has its type and help", note)
		}
	})
}

// TestSeriesMetadataDescriptionQuotesLookback keeps the model-facing text
// honest: the window it promises has to be the window the code applies.
func TestSeriesMetadataDescriptionQuotesLookback(t *testing.T) {
	if want := shortDuration(MetadataLookback); !strings.Contains(seriesMetadataDescription, want) {
		t.Errorf("series_metadata description does not mention the %s discovery window: %q", want, seriesMetadataDescription)
	}
}

// --- shared helpers --------------------------------------------------------

func TestTruncateString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under cap unchanged", "short", 120, "short"},
		{"at cap unchanged", strings.Repeat("a", 120), 120, strings.Repeat("a", 120)},
		{"over cap truncated", strings.Repeat("a", 121), 120, strings.Repeat("a", 117) + "…"},
		{"multibyte boundary respected", strings.Repeat("é", 70), 120, strings.Repeat("é", 58) + "…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateString(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("truncateString(len %d, max %d) = %q, want %q", len(tc.in), tc.max, got, tc.want)
			}
			if len(got) > tc.max {
				t.Errorf("result is %d bytes, exceeds max %d", len(got), tc.max)
			}
		})
	}
}

// TestByteSize pins that a cap quoted to the model is the cap that applies.
// Integer division alone floors, so a non-multiple of 1024 would be described
// as a smaller limit than the one actually enforced.
func TestByteSize(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{MaxResponseBytes, "32 KiB"},
		{1024, "1 KiB"},
		{40000, "40000 bytes"},
		{1023, "1023 bytes"},
		{0, "0 KiB"},
	}
	for _, tc := range tests {
		if got := byteSize(tc.in); got != tc.want {
			t.Errorf("byteSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLabelsetKeyIsUnambiguous pins the one property the final ranking
// tie-break depends on: distinct label sets must never share a key. Raw
// concatenation collides whenever a value contains the separators, which
// silently turns the tie-break into a no-op.
func TestLabelsetKeyIsUnambiguous(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]string
	}{
		{"separators inside a value", map[string]string{"a": "b,c=d"}, map[string]string{"a": "b", "c": "d"}},
		{"equals inside a value", map[string]string{"a": "b=c"}, map[string]string{"a=b": "c"}},
		{"trailing empty value", map[string]string{"a": "b,c="}, map[string]string{"a": "b", "c": ""}},
		// Guards the fix itself: a quoted key must escape embedded quotes,
		// or the collision simply moves to values that contain one.
		{"quotes inside a value", map[string]string{"a": `b","c`, "d": "e"}, map[string]string{"a": "b", "c": "", "d": "e"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if ka, kb := labelsetKey(tc.a), labelsetKey(tc.b); ka == kb {
				t.Errorf("labelsetKey(%v) == labelsetKey(%v) == %q; distinct label sets must not collide", tc.a, tc.b, ka)
			}
		})
	}
}

// ramp builds n points whose timestamps are their indices, so a thinned
// result reads back as the list of retained indices.
func ramp(n int) []point {
	pts := make([]point, 0, n)
	for i := 0; i < n; i++ {
		pts = append(pts, point{ts: int64(i), val: strconv.Itoa(i)})
	}
	return pts
}

// retained lists the timestamps of pts, i.e. the original indices under ramp.
func retained(pts []point) []int64 {
	out := make([]int64, 0, len(pts))
	for _, p := range pts {
		out = append(out, p.ts)
	}
	return out
}

// TestThinPointsRetainedPattern pins exactly which samples survive one pass,
// for the representative sizes documented on thinPoints. It is the sampling
// half of the contract: the stride change of issue #33 moved every retained
// index, not just the ones on oversized results, so the pattern is worth
// asserting rather than inferring from a length.
func TestThinPointsRetainedPattern(t *testing.T) {
	evens := func(n int) []int64 {
		var out []int64
		for i := 0; i < n; i += 2 {
			out = append(out, int64(i))
		}
		return out
	}
	tests := []struct {
		n           int
		want        []int64
		wantChanged bool
	}{
		{0, nil, false},
		{1, []int64{0}, false},
		{2, []int64{0, 1}, false},
		// The case the fixed point used to swallow: three points must reach
		// the two-point floor the spec's keep-first-and-last rule describes.
		{3, []int64{0, 2}, true},
		{4, []int64{0, 2, 3}, true},
		{5, []int64{0, 2, 4}, true},
		// Even n: uniform except for the short final gap forced by pinning
		// the last sample.
		{8, []int64{0, 2, 4, 6, 7}, true},
		// Odd n: one pass always yields an evenly spaced grid. The count
		// does not stay odd, though — n=7 retains 4 — so the chain is
		// uniform all the way down only for n = 2^j+1, of which 33 is one
		// and 7 is not. See TestThinPointsChainSpacing.
		{7, []int64{0, 2, 4, 6}, true},
		{9, []int64{0, 2, 4, 6, 8}, true},
		{33, evens(33), true},
	}
	for _, tc := range tests {
		t.Run(strconv.Itoa(tc.n), func(t *testing.T) {
			got, changed := thinPoints(ramp(tc.n))
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if fmt.Sprint(retained(got)) != fmt.Sprint(tc.want) {
				t.Errorf("retained indices = %v, want %v", retained(got), tc.want)
			}
		})
	}
}

// TestThinPointsConvergesToTwoPoints is the termination half: every series
// above the floor must shrink on every pass and land on exactly two points,
// with the endpoints intact. Before issue #33 the stride started at index 1,
// which kept index 1 of a 3-point series and stalled there — one point above
// the floor, and enough to make the query_range size backstop give up while a
// fitting payload was still reachable.
func TestThinPointsConvergesToTwoPoints(t *testing.T) {
	for n := 3; n <= 2*MaxPointsPerSeries; n++ {
		pts := ramp(n)
		last := len(pts)
		for passes := 0; ; passes++ {
			if passes > n {
				t.Fatalf("n=%d: still thinning after %d passes, at %d points", n, passes, len(pts))
			}
			next, changed := thinPoints(pts)
			if !changed {
				break
			}
			if len(next) >= last {
				t.Fatalf("n=%d: pass %d went from %d to %d points; thinning must shrink", n, passes, last, len(next))
			}
			pts, last = next, len(next)
		}
		if len(pts) != 2 {
			t.Errorf("n=%d converged at %d points, want the two-point floor", n, len(pts))
		}
		if pts[0].ts != 0 || pts[len(pts)-1].ts != int64(n-1) {
			t.Errorf("n=%d: first/last = %d/%d, want 0/%d", n, pts[0].ts, pts[len(pts)-1].ts, n-1)
		}
	}
}

// TestThinPointsChainSpacing pins what repeated thinning does to the spacing
// of the survivors — the claim the stride change has to be judged on, since it
// moves the retained indices for every n, not only for oversized results.
//
// Two properties, both asserted for every n up to twice MaxPointsPerSeries:
//
//   - every interior gap is equal, i.e. the survivors are a uniform grid, and
//   - the one gap that may differ is the last, and it is never longer.
//
// The second is what makes the sampling defensible when the chain is not
// perfectly uniform: the deviation sits at the newest end of the window and
// always oversamples it slightly, so thinning never blurs the most recent
// samples more than the average. A stride that spread the deviation through
// the middle, or that undersampled the tail, would pass the convergence test
// above and still be the wrong sampling.
//
// The chain of a full-window query (MaxPointsPerSeries and effectiveStep clamp
// it to 121 points) is pinned explicitly, because the doc comment on
// thinPoints quotes it as the canonical case.
func TestThinPointsChainSpacing(t *testing.T) {
	for n := 3; n <= 2*MaxPointsPerSeries; n++ {
		pts := ramp(n)
		for pass := 1; ; pass++ {
			next, changed := thinPoints(pts)
			if !changed {
				break
			}
			pts = next
			var regular, tail int64
			for i := 1; i < len(pts); i++ {
				gap := pts[i].ts - pts[i-1].ts
				switch {
				case i == len(pts)-1:
					tail = gap
				case regular == 0:
					regular = gap
				case gap != regular:
					t.Fatalf("n=%d pass %d: interior gaps are not uniform (%d then %d) in %v",
						n, pass, regular, gap, retained(pts))
				}
			}
			if regular != 0 && tail > regular {
				t.Errorf("n=%d pass %d: final gap %d exceeds the interior gap %d in %v; the newest samples must not be thinned harder than the rest",
					n, pass, tail, regular, retained(pts))
			}
		}
	}

	var chain []int
	for pts := ramp(MaxPointsPerSeries + 1); ; {
		chain = append(chain, len(pts))
		next, changed := thinPoints(pts)
		if !changed {
			break
		}
		pts = next
	}
	want := []int{121, 61, 31, 16, 9, 5, 3, 2}
	if fmt.Sprint(chain) != fmt.Sprint(want) {
		t.Errorf("121-point chain = %v, want %v", chain, want)
	}
}
