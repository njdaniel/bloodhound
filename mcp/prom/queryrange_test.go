package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resultText extracts the single text content block of a tool result.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil {
		t.Fatal("nil CallToolResult")
	}
	if len(res.Content) != 1 {
		t.Fatalf("got %d content blocks, want exactly 1", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content block is %T, want *mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

// gotRange mirrors the query_range output payload for assertions.
type gotRange struct {
	ResolvedStepSeconds int64 `json:"resolved_step_seconds"`
	Series              []struct {
		Labels map[string]string `json:"labels"`
		Stats  struct {
			Min  float64 `json:"min"`
			Max  float64 `json:"max"`
			Avg  float64 `json:"avg"`
			Last float64 `json:"last"`
		} `json:"stats"`
		Points [][]any `json:"points"`
	} `json:"series"`
	Truncation struct {
		SeriesTotal    int    `json:"series_total"`
		SeriesReturned int    `json:"series_returned"`
		PointsThinned  bool   `json:"points_thinned"`
		Note           string `json:"note"`
	} `json:"truncation"`
}

func decodeRange(t *testing.T, res *mcp.CallToolResult) gotRange {
	t.Helper()
	var g gotRange
	if err := json.Unmarshal([]byte(resultText(t, res)), &g); err != nil {
		t.Fatalf("decoding query_range payload: %v", err)
	}
	return g
}

func TestEffectiveStep(t *testing.T) {
	base := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		window    time.Duration
		requested int64
		want      int64
	}{
		{"1h omitted step", time.Hour, 0, 30},
		{"1h step below clamp", time.Hour, 15, 30},
		{"1h step at clamp", time.Hour, 30, 30},
		{"1h step above clamp", time.Hour, 60, 60},
		{"24h omitted step", 24 * time.Hour, 0, 720},
		{"24h coarse step kept", 24 * time.Hour, 3600, 3600},
		{"10m step clamped up", 10 * time.Minute, 1, 5},
		{"2m omitted step floors at 1", 2 * time.Minute, 0, 1},
		{"30s omitted step floors at 1", 30 * time.Second, 0, 1},
		{"121s rounds ceil to 2", 121 * time.Second, 0, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveStep(base, base.Add(tc.window), tc.requested)
			if got != tc.want {
				t.Errorf("effectiveStep(%v, requested=%d) = %d, want %d", tc.window, tc.requested, got, tc.want)
			}
		})
	}
}

func TestQueryRangeEchoesResolvedStepAndSendsItUpstream(t *testing.T) {
	fake := newFakeProm(t)
	ts := newTestToolServer(fake)

	res, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "up",
		Start: "2026-07-28T10:00:00Z",
		End:   "2026-07-28T11:00:00Z",
		// step 15s over 1h would be 240 points; must clamp to 30s.
		StepSeconds: 15,
	})
	if err != nil {
		t.Fatalf("handleQueryRange: %v", err)
	}
	g := decodeRange(t, res)
	if g.ResolvedStepSeconds != 30 {
		t.Errorf("resolved_step_seconds = %d, want 30", g.ResolvedStepSeconds)
	}
	if got := fake.lastParams("/api/v1/query_range").Get("step"); got != "30" {
		t.Errorf("step sent upstream = %q, want \"30\"", got)
	}
}

// matrixJSON builds a canned /api/v1/query_range success body.
func matrixJSON(t *testing.T, series []map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "matrix", "result": series},
	})
	if err != nil {
		t.Fatalf("building matrix fixture: %v", err)
	}
	return string(body)
}

// seriesFixture builds one matrix series map for matrixJSON.
func seriesFixture(labels map[string]string, values [][2]any) map[string]any {
	vs := make([]any, 0, len(values))
	for _, v := range values {
		vs = append(vs, []any{v[0], v[1]})
	}
	m := map[string]any{"metric": labels, "values": vs}
	return m
}

func TestQueryRangeSeriesRankingAndTieBreaks(t *testing.T) {
	fake := newFakeProm(t)
	// Four series:
	//   volatile: range 10, should come first.
	//   Three equal-range (2), equal-max-abs (3) series that must fall back
	//   to lexicographic sorted-labelset order: pod=a, pod=b, pod=c.
	fake.set("/api/v1/query_range", 200, matrixJSON(t, []map[string]any{
		seriesFixture(map[string]string{"pod": "c"}, [][2]any{{1753700000, "1"}, {1753700030, "3"}}),
		seriesFixture(map[string]string{"pod": "a"}, [][2]any{{1753700000, "1"}, {1753700030, "3"}}),
		seriesFixture(map[string]string{"pod": "volatile"}, [][2]any{{1753700000, "0"}, {1753700030, "10"}}),
		seriesFixture(map[string]string{"pod": "b"}, [][2]any{{1753700000, "1"}, {1753700030, "3"}}),
	}))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "up", Start: "2026-07-28T10:00:00Z", End: "2026-07-28T10:01:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryRange: %v", err)
	}
	g := decodeRange(t, res)
	var order []string
	for _, s := range g.Series {
		order = append(order, s.Labels["pod"])
	}
	want := []string{"volatile", "a", "b", "c"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("series order = %v, want %v", order, want)
	}
}

func TestQueryRangeMaxAbsTieBreakBeforeLabelset(t *testing.T) {
	fake := newFakeProm(t)
	// Both series have range 10; "low" has max |value| 5, "high" has 10.
	// Labelset order alone would put "high" second — max-abs must win first.
	fake.set("/api/v1/query_range", 200, matrixJSON(t, []map[string]any{
		seriesFixture(map[string]string{"pod": "a-low"}, [][2]any{{1753700000, "-5"}, {1753700030, "5"}}),
		seriesFixture(map[string]string{"pod": "b-high"}, [][2]any{{1753700000, "0"}, {1753700030, "10"}}),
	}))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "up", Start: "2026-07-28T10:00:00Z", End: "2026-07-28T10:01:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryRange: %v", err)
	}
	g := decodeRange(t, res)
	if g.Series[0].Labels["pod"] != "b-high" {
		t.Errorf("first series = %q, want b-high (max |value| tie-break)", g.Series[0].Labels["pod"])
	}
}

func TestQueryRangeSeriesCap(t *testing.T) {
	fake := newFakeProm(t)
	var series []map[string]any
	for i := 0; i < MaxSeries+2; i++ {
		// Distinct ranges so the kept set is unambiguous: higher i, higher range.
		series = append(series, seriesFixture(
			map[string]string{"pod": fmt.Sprintf("pod-%02d", i)},
			[][2]any{{1753700000, "0"}, {1753700030, fmt.Sprintf("%d", i)}},
		))
	}
	fake.set("/api/v1/query_range", 200, matrixJSON(t, series))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "up", Start: "2026-07-28T10:00:00Z", End: "2026-07-28T10:01:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryRange: %v", err)
	}
	g := decodeRange(t, res)
	if g.Truncation.SeriesTotal != MaxSeries+2 || g.Truncation.SeriesReturned != MaxSeries {
		t.Errorf("truncation counts = %d/%d, want %d/%d",
			g.Truncation.SeriesReturned, g.Truncation.SeriesTotal, MaxSeries, MaxSeries+2)
	}
	if len(g.Series) != MaxSeries {
		t.Fatalf("got %d series, want %d", len(g.Series), MaxSeries)
	}
	// Lowest-range series (pod-00, pod-01) must be the ones dropped.
	for _, s := range g.Series {
		if s.Labels["pod"] == "pod-00" || s.Labels["pod"] == "pod-01" {
			t.Errorf("low-volatility series %q survived the cap", s.Labels["pod"])
		}
	}
	if !strings.Contains(g.Truncation.Note, "2 series dropped") ||
		!strings.Contains(g.Truncation.Note, "(max-min) descending") {
		t.Errorf("truncation note %q missing count or ranking rule", g.Truncation.Note)
	}
}

func TestQueryRangeSizeBackstop(t *testing.T) {
	fake := newFakeProm(t)
	// 15 series x 121 points is comfortably over 32 KiB serialized. A spike
	// at interior index 2 (dropped by the first thinning pass) proves stats
	// come from full-resolution data.
	const nPoints = 121
	var series []map[string]any
	for i := 0; i < MaxSeries; i++ {
		values := make([][2]any, 0, nPoints)
		for p := 0; p < nPoints; p++ {
			v := fmt.Sprintf("%d.5", i*1000+p)
			if p == 2 {
				v = "99999"
			}
			values = append(values, [2]any{1753700000 + p*30, v})
		}
		series = append(series, seriesFixture(
			map[string]string{"pod": fmt.Sprintf("checkout-%02d", i), "namespace": "shop"}, values))
	}
	fake.set("/api/v1/query_range", 200, matrixJSON(t, series))
	ts := newTestToolServer(fake)

	res, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "up", Start: "2026-07-28T10:00:00Z", End: "2026-07-28T11:00:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryRange: %v", err)
	}
	payload := resultText(t, res)
	if len(payload) > MaxResponseBytes {
		t.Errorf("payload is %d bytes, want <= %d", len(payload), MaxResponseBytes)
	}
	g := decodeRange(t, res)
	if !g.Truncation.PointsThinned {
		t.Fatal("points_thinned = false, want true")
	}
	if !strings.Contains(g.Truncation.Note, "thinned") {
		t.Errorf("truncation note %q does not mention thinning", g.Truncation.Note)
	}
	for i, s := range g.Series {
		if len(s.Points) >= nPoints {
			t.Fatalf("series %d not thinned: %d points", i, len(s.Points))
		}
		first, last := s.Points[0], s.Points[len(s.Points)-1]
		if first[0].(float64) != 1753700000 || last[0].(float64) != 1753700000+(nPoints-1)*30 {
			t.Errorf("series %d first/last timestamps = %v/%v, want originals retained", i, first[0], last[0])
		}
		if s.Stats.Max != 99999 {
			t.Errorf("series %d stats.max = %v, want 99999 (full-resolution, spike was thinned away)", i, s.Stats.Max)
		}
	}
}

// TestQueryRangeCollidingLabelsetsRankDeterministically feeds two series that
// tie on every ranking key and whose label sets render identically under naive
// concatenation ({a: "b,c=d"} vs {a: "b", c: "d"}). If the labelset tie-break
// cannot tell them apart, the sort keeps whatever order Prometheus returned,
// so the same result set serialized in two upstream orders yields two
// different payloads.
func TestQueryRangeCollidingLabelsetsRankDeterministically(t *testing.T) {
	// Identical value range (5) and max |value| (5): the labelset key is the
	// only thing left to order them by.
	merged := seriesFixture(map[string]string{"a": "b,c=d"},
		[][2]any{{1753700000, "0"}, {1753700030, "5"}})
	split := seriesFixture(map[string]string{"a": "b", "c": "d"},
		[][2]any{{1753700000, "0"}, {1753700030, "5"}})

	run := func(t *testing.T, upstream []map[string]any) string {
		t.Helper()
		fake := newFakeProm(t)
		fake.set("/api/v1/query_range", 200, matrixJSON(t, upstream))
		res, _, err := newTestToolServer(fake).handleQueryRange(context.Background(), nil, queryRangeInput{
			Query: "up", Start: "2026-07-28T10:00:00Z", End: "2026-07-28T10:01:00Z",
		})
		if err != nil {
			t.Fatalf("handleQueryRange: %v", err)
		}
		return resultText(t, res)
	}

	first := run(t, []map[string]any{merged, split})
	second := run(t, []map[string]any{split, merged})
	if first != second {
		t.Errorf("upstream order changed the payload; the labelset tie-break is not total:\n a,b: %s\n b,a: %s", first, second)
	}
}

func TestQueryRangeRangeLimitError(t *testing.T) {
	ts := newTestToolServer(newFakeProm(t))
	_, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "up", Start: "2026-07-28T00:00:00Z", End: "2026-07-29T12:00:00Z",
	})
	if err == nil {
		t.Fatal("36h window: got nil error, want range-limit error")
	}
	for _, want := range []string{"36h", "24h", "narrow"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestQueryRangeBadPromQLError(t *testing.T) {
	fake := newFakeProm(t)
	fake.set("/api/v1/query_range", 400,
		`{"status":"error","errorType":"bad_data","error":"invalid parameter \"query\": 1:4: parse error: unexpected identifier \"foo\""}`)
	ts := newTestToolServer(fake)

	_, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "up foo", Start: "2026-07-28T10:00:00Z", End: "2026-07-28T11:00:00Z",
	})
	if err == nil {
		t.Fatal("bad PromQL: got nil error, want upstream error")
	}
	if !strings.Contains(err.Error(), "parse error") || !strings.Contains(err.Error(), "bad_data") {
		t.Errorf("error %q should carry the upstream parse error and type", err)
	}
}

func TestQueryRangeEndNotAfterStartError(t *testing.T) {
	ts := newTestToolServer(newFakeProm(t))
	_, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "up", Start: "2026-07-28T11:00:00Z", End: "2026-07-28T10:00:00Z",
	})
	if err == nil || !strings.Contains(err.Error(), "not after") {
		t.Errorf("got %v, want end-not-after-start error", err)
	}
}
