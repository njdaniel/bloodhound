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
	if len(desc) != MaxAnnotationLen || !strings.HasSuffix(desc, "…") {
		t.Errorf("annotation length = %d (ellipsis: %v), want %d bytes ending in …",
			len(desc), strings.HasSuffix(desc, "…"), MaxAnnotationLen)
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

// TestSeriesMetadataMarksUpstreamSeriesCap covers the case where the server
// honours the limit: the caps the model is told about must include the one
// applied upstream, or it reads a partial discovery as a complete one.
func TestSeriesMetadataMarksUpstreamSeriesCap(t *testing.T) {
	fake := newFakeProm(t)
	sets := make([]map[string]string, 0, MaxUpstreamSeries)
	for i := 0; i < MaxUpstreamSeries; i++ {
		sets = append(sets, map[string]string{"__name__": "metric_00", "pod": fmt.Sprintf("pod-%04d", i)})
	}
	fake.set("/api/v1/series", 200, seriesJSON(t, sets))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleSeriesMetadata(context.Background(), nil, seriesMetadataInput{Match: `{__name__=~".+"}`})
	if err != nil {
		t.Fatalf("handleSeriesMetadata: %v", err)
	}
	if note := decodeMetadata(t, res).Truncation.Note; !strings.Contains(note, "upstream series lookup") {
		t.Errorf("truncation note %q does not report the upstream series cap", note)
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

func TestThinPointsKeepsFirstAndLast(t *testing.T) {
	pts := []point{{0, "a"}, {1, "b"}, {2, "c"}, {3, "d"}, {4, "e"}}
	got, changed := thinPoints(pts)
	if !changed {
		t.Fatal("thinPoints reported no change")
	}
	if got[0].ts != 0 || got[len(got)-1].ts != 4 {
		t.Errorf("first/last = %d/%d, want 0/4", got[0].ts, got[len(got)-1].ts)
	}
	if len(got) != 4 { // p0, p1, p3, p4
		t.Errorf("got %d points, want 4", len(got))
	}
	short := []point{{0, "a"}, {1, "b"}}
	if _, changed := thinPoints(short); changed {
		t.Error("thinPoints changed a 2-point series")
	}
}
