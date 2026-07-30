package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolServer implements the four mcp-prom tool handlers on top of a
// promClient. Handlers return errors for anything the model can act on (bad
// PromQL, too-wide windows); the SDK surfaces those as MCP tool errors, not
// protocol failures.
type toolServer struct {
	prom *promClient
}

// textResult wraps a serialized JSON payload as the single MCP text content
// block every tool returns.
func textResult(payload []byte) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}},
	}
}

// parseRFC3339 parses an RFC 3339 timestamp, naming the field in errors so
// the model knows which argument to fix.
func parseRFC3339(field, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing %q: %q is not RFC 3339 (want e.g. 2026-07-28T10:15:00Z): %w", field, value, err)
	}
	return t, nil
}

// shortDuration renders a duration compactly for error messages
// (36h0m0s → 36h).
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// --- query_range -----------------------------------------------------------

// queryRangeInput is the input payload for the query_range tool.
type queryRangeInput struct {
	Query       string `json:"query"`
	Start       string `json:"start"`
	End         string `json:"end"`
	StepSeconds int64  `json:"step_seconds,omitempty"`
}

// rangeSeries is one output series: labels, full-resolution stats, and
// (possibly thinned) points.
type rangeSeries struct {
	Labels map[string]string `json:"labels"`
	Stats  seriesStats       `json:"stats"`
	Points []point           `json:"points"`
}

// rangeTruncation reports every cap applied to a query_range result.
type rangeTruncation struct {
	SeriesTotal    int    `json:"series_total"`
	SeriesReturned int    `json:"series_returned"`
	PointsThinned  bool   `json:"points_thinned"`
	Note           string `json:"note,omitempty"`
}

// rangeResult is the JSON payload of a query_range tool result.
type rangeResult struct {
	ResolvedStepSeconds int64           `json:"resolved_step_seconds"`
	Series              []rangeSeries   `json:"series"`
	Truncation          rangeTruncation `json:"truncation"`
}

// effectiveStep clamps the requested step so a series has at most
// MaxPointsPerSeries points: max(step_seconds, ceil((end−start)/120)), with
// the same ceil default when the step is omitted (spec 002 §2.3 step 2).
func effectiveStep(start, end time.Time, requested int64) int64 {
	rangeSecs := int64(math.Ceil(end.Sub(start).Seconds()))
	minStep := (rangeSecs + MaxPointsPerSeries - 1) / MaxPointsPerSeries
	if minStep < 1 {
		minStep = 1
	}
	if requested > minStep {
		return requested
	}
	return minStep
}

// handleQueryRange implements the query_range tool, applying the exact
// truncation strategy of spec 002 §2.3 in order: range check, step clamp,
// query, series cap, value formatting, size backstop.
func (s *toolServer) handleQueryRange(ctx context.Context, _ *mcp.CallToolRequest, in queryRangeInput) (*mcp.CallToolResult, any, error) {
	start, err := parseRFC3339("start", in.Start)
	if err != nil {
		return nil, nil, err
	}
	end, err := parseRFC3339("end", in.End)
	if err != nil {
		return nil, nil, err
	}
	if !end.After(start) {
		return nil, nil, fmt.Errorf("end %s is not after start %s; swap or widen the window", in.End, in.Start)
	}
	// 1. Range check: an error, never a silent clamp — a silently shrunk
	// window would make the model reason about a different time span than
	// it asked for.
	if window := end.Sub(start); window > MaxRange {
		return nil, nil, fmt.Errorf("range %s exceeds %s limit; narrow start/end and retry",
			shortDuration(window), shortDuration(MaxRange))
	}

	// 2. Step clamp.
	step := effectiveStep(start, end, in.StepSeconds)

	// 3. Query with timeout (enforced inside promClient).
	upstream, err := s.prom.queryRange(ctx, in.Query, start, end, step)
	if err != nil {
		return nil, nil, err
	}

	// Shape every series; stats and ranking keys come from full-resolution
	// data before any thinning.
	ranked := make([]rankedSeries, 0, len(upstream))
	for _, ps := range upstream {
		stats, valueRange, maxAbs := computeStats(ps.Values)
		pts := make([]point, 0, len(ps.Values))
		for _, p := range ps.Values {
			pts = append(pts, point{ts: int64(math.Round(p.T)), val: formatValue(p.V)})
		}
		ranked = append(ranked, rankedSeries{
			series: rangeSeries{
				Labels: truncateLabels(ps.Metric),
				Stats:  stats,
				Points: pts,
			},
			valueRange: valueRange,
			maxAbs:     maxAbs,
			key:        labelsetKey(ps.Metric),
		})
	}

	// 4. Series cap: rank by (max−min) desc, tie-break max |value| desc,
	// then lexicographic sorted labelset; keep the top MaxSeries.
	rankSeries(ranked)
	total := len(ranked)
	if total > MaxSeries {
		ranked = ranked[:MaxSeries]
	}
	series := make([]rangeSeries, 0, len(ranked))
	for _, r := range ranked {
		series = append(series, r.series)
	}

	// One sentence per applied cap, in the order §2.3 applies them. The
	// series cap is settled here; the size backstop below appends to it.
	var notes []string
	if total > MaxSeries {
		notes = append(notes, fmt.Sprintf("%d series dropped; ranked by (max-min) descending. Narrow the selector to see specific series.", total-MaxSeries))
	}
	preSizeNotes := len(notes) // notes[:preSizeNotes] survive every backstop pass
	res := rangeResult{
		ResolvedStepSeconds: step,
		Series:              series,
		Truncation: rangeTruncation{
			SeriesTotal:    total,
			SeriesReturned: len(series),
			PointsThinned:  false,
			Note:           joinNotes(notes...),
		},
	}

	// 5. Value formatting happened above (unix-second timestamps, FloatFormat
	// strings). 6. Size backstop: thin interior points, keeping first and
	// last, until the payload fits; stats stay full-resolution.
	payload, err := json.Marshal(res)
	if err != nil {
		return nil, nil, fmt.Errorf("serializing query_range result: %w", err)
	}
	for len(payload) > MaxResponseBytes {
		changed := false
		maxPts := 0
		for i := range res.Series {
			thinned, ch := thinPoints(res.Series[i].Points)
			if ch {
				res.Series[i].Points = thinned
				changed = true
			}
			if len(res.Series[i].Points) > maxPts {
				maxPts = len(res.Series[i].Points)
			}
		}
		if !changed {
			// Thinning has reached its floor and the payload still does not
			// fit, so it ships oversized. Say so: an unmarked overflow is the
			// one thing §2.1 rules out, because the model would read a capped
			// result as a complete one. The note itself adds bytes to an
			// already-oversized payload, which is the cheaper cost.
			//
			// Since issue #33 the floor is what the spec says it is:
			// thinPoints shrinks every series of three or more points, so
			// reaching here means every series is down to its first and last
			// sample and maxPts is at most 2. What is left is labels, stats
			// and series count — none of which this step can thin — so the
			// advice points at the selector.
			notes = append(notes, fmt.Sprintf("Result exceeds the %s response cap and no further thinning is possible: every series is already reduced to its first and last points (at most %d per series remain); it is returned oversized. The remaining size is labels and series count, not points, so narrow the selector.",
				byteSize(MaxResponseBytes), maxPts))
			res.Truncation.Note = joinNotes(notes...)
			payload, err = json.Marshal(res)
			if err != nil {
				return nil, nil, fmt.Errorf("serializing oversized query_range result: %w", err)
			}
			break
		}
		res.Truncation.PointsThinned = true
		notes = append(notes[:preSizeNotes],
			fmt.Sprintf("Points thinned to at most %d per series to fit the %s response cap; first and last points kept; stats reflect full resolution.", maxPts, byteSize(MaxResponseBytes)))
		res.Truncation.Note = joinNotes(notes...)
		payload, err = json.Marshal(res)
		if err != nil {
			return nil, nil, fmt.Errorf("serializing thinned query_range result: %w", err)
		}
	}
	return textResult(payload), nil, nil
}

// --- query_instant ---------------------------------------------------------

// queryInstantInput is the input payload for the query_instant tool.
type queryInstantInput struct {
	Query string `json:"query"`
	Time  string `json:"time,omitempty"`
}

// instantSample is one output sample of a query_instant result.
type instantSample struct {
	Labels    map[string]string `json:"labels"`
	Value     string            `json:"value"`
	Timestamp int64             `json:"timestamp"`
}

// instantTruncation reports the sample cap applied to a query_instant result.
type instantTruncation struct {
	SamplesTotal    int    `json:"samples_total"`
	SamplesReturned int    `json:"samples_returned"`
	Note            string `json:"note,omitempty"`
}

// instantResult is the JSON payload of a query_instant tool result.
type instantResult struct {
	ResultType string            `json:"result_type"`
	Samples    []instantSample   `json:"samples"`
	Truncation instantTruncation `json:"truncation"`
}

// handleQueryInstant implements the query_instant tool: at most
// MaxInstantSamples samples, ranked by |value| descending (outliers first)
// with a deterministic tie-break on the sorted labelset.
func (s *toolServer) handleQueryInstant(ctx context.Context, _ *mcp.CallToolRequest, in queryInstantInput) (*mcp.CallToolResult, any, error) {
	var ts time.Time
	if in.Time != "" {
		var err error
		ts, err = parseRFC3339("time", in.Time)
		if err != nil {
			return nil, nil, err
		}
	}
	resultType, upstream, err := s.prom.query(ctx, in.Query, ts)
	if err != nil {
		return nil, nil, err
	}

	type rankedSample struct {
		sample instantSample
		abs    float64 // |value|; NaN ranks last
		key    string  // sorted labelset, ascending tie-break
	}
	ranked := make([]rankedSample, 0, len(upstream))
	for _, ps := range upstream {
		abs := math.Abs(ps.Value.V)
		if math.IsNaN(abs) {
			abs = -1
		}
		ranked = append(ranked, rankedSample{
			sample: instantSample{
				Labels:    truncateLabels(ps.Metric),
				Value:     formatValue(ps.Value.V),
				Timestamp: int64(math.Round(ps.Value.T)),
			},
			abs: abs,
			key: labelsetKey(ps.Metric),
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].abs != ranked[j].abs {
			return ranked[i].abs > ranked[j].abs
		}
		return ranked[i].key < ranked[j].key
	})
	total := len(ranked)
	if total > MaxInstantSamples {
		ranked = ranked[:MaxInstantSamples]
	}
	samples := make([]instantSample, 0, len(ranked))
	for _, r := range ranked {
		samples = append(samples, r.sample)
	}

	var note string
	if total > MaxInstantSamples {
		note = fmt.Sprintf("%d samples dropped; ranked by |value| descending. Narrow the selector to see specific series.", total-MaxInstantSamples)
	}
	payload, err := json.Marshal(instantResult{
		ResultType: resultType,
		Samples:    samples,
		Truncation: instantTruncation{
			SamplesTotal:    total,
			SamplesReturned: len(samples),
			Note:            note,
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("serializing query_instant result: %w", err)
	}
	return textResult(payload), nil, nil
}

// --- list_alerts -----------------------------------------------------------

// listAlertsInput is the input payload for the list_alerts tool.
type listAlertsInput struct {
	State string `json:"state,omitempty"`
}

// alertOut is one output alert of a list_alerts result.
type alertOut struct {
	Name        string            `json:"name"`
	State       string            `json:"state"`
	ActiveAt    string            `json:"active_at"`
	Value       string            `json:"value"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

// alertsTruncation reports the alert cap applied to a list_alerts result.
type alertsTruncation struct {
	AlertsTotal    int    `json:"alerts_total"`
	AlertsReturned int    `json:"alerts_returned"`
	Note           string `json:"note,omitempty"`
}

// alertsResult is the JSON payload of a list_alerts tool result.
type alertsResult struct {
	Alerts     []alertOut       `json:"alerts"`
	Truncation alertsTruncation `json:"truncation"`
}

// handleListAlerts implements the list_alerts tool: filtered by state
// (default firing), sorted by active_at descending, capped at MaxAlerts,
// annotation values truncated to MaxAnnotationLen.
func (s *toolServer) handleListAlerts(ctx context.Context, _ *mcp.CallToolRequest, in listAlertsInput) (*mcp.CallToolResult, any, error) {
	state := in.State
	if state == "" {
		state = "firing"
	}
	upstream, err := s.prom.alerts(ctx)
	if err != nil {
		return nil, nil, err
	}

	type rankedAlert struct {
		alert    alertOut
		activeAt time.Time
		key      string // sorted labelset, ascending tie-break
	}
	ranked := make([]rankedAlert, 0, len(upstream))
	for _, a := range upstream {
		if state != "all" && a.State != state {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, a.ActiveAt)
		if err != nil {
			at = time.Time{} // unparseable activeAt sorts last, deterministically
		}
		annotations := make(map[string]string, len(a.Annotations))
		for k, v := range a.Annotations {
			annotations[k] = truncateString(v, MaxAnnotationLen)
		}
		value := a.Value
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			value = formatValue(f)
		}
		ranked = append(ranked, rankedAlert{
			alert: alertOut{
				Name:        a.Labels["alertname"],
				State:       a.State,
				ActiveAt:    a.ActiveAt,
				Value:       value,
				Labels:      truncateLabels(a.Labels),
				Annotations: annotations,
			},
			activeAt: at,
			key:      labelsetKey(a.Labels),
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if !ranked[i].activeAt.Equal(ranked[j].activeAt) {
			return ranked[i].activeAt.After(ranked[j].activeAt)
		}
		return ranked[i].key < ranked[j].key
	})
	total := len(ranked)
	if total > MaxAlerts {
		ranked = ranked[:MaxAlerts]
	}
	alerts := make([]alertOut, 0, len(ranked))
	for _, r := range ranked {
		alerts = append(alerts, r.alert)
	}

	var note string
	if total > MaxAlerts {
		note = fmt.Sprintf("%d alerts dropped; sorted by active_at descending (newest kept).", total-MaxAlerts)
	}
	payload, err := json.Marshal(alertsResult{
		Alerts: alerts,
		Truncation: alertsTruncation{
			AlertsTotal:    total,
			AlertsReturned: len(alerts),
			Note:           note,
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("serializing list_alerts result: %w", err)
	}
	return textResult(payload), nil, nil
}

// --- series_metadata -------------------------------------------------------

// seriesMetadataInput is the input payload for the series_metadata tool.
type seriesMetadataInput struct {
	Match string `json:"match"`
}

// metricMeta is one output metric of a series_metadata result.
type metricMeta struct {
	Name   string              `json:"name"`
	Type   string              `json:"type"`
	Help   string              `json:"help"`
	Labels map[string][]string `json:"labels"`
}

// metadataTruncation reports the metric cap applied to a series_metadata
// result.
type metadataTruncation struct {
	MetricsTotal    int    `json:"metrics_total"`
	MetricsReturned int    `json:"metrics_returned"`
	Note            string `json:"note,omitempty"`
}

// metadataResult is the JSON payload of a series_metadata tool result.
type metadataResult struct {
	Metrics    []metricMeta       `json:"metrics"`
	Truncation metadataTruncation `json:"truncation"`
}

// handleSeriesMetadata implements the series_metadata tool, backed by
// /api/v1/series (which metrics and labels match the selector) plus
// /api/v1/metadata (type and help text). At most MaxMetadataMetrics metrics,
// alphabetical — this is a discovery tool, so determinism beats relevance
// ranking — with at most MaxLabelValues sample values per label key.
//
// Both upstream calls are bounded as well as the output: the series lookup
// covers only the last MetadataLookback and asks for at most
// MaxUpstreamSeries series, and the metadata lookup asks for at most
// MaxUpstreamMetadata metrics. Otherwise a discovery call that returns 25
// metrics makes Prometheus scan its full retention and describe every metric
// it knows.
func (s *toolServer) handleSeriesMetadata(ctx context.Context, _ *mcp.CallToolRequest, in seriesMetadataInput) (*mcp.CallToolResult, any, error) {
	end := time.Now()
	start := end.Add(-MetadataLookback)
	sets, warnings, err := s.prom.series(ctx, in.Match, start, end, MaxUpstreamSeries)
	if err != nil {
		return nil, nil, err
	}
	// Whether the server truncated is answered by the warnings, not by the
	// result count: a server old enough to ignore limit returns everything,
	// and counting would then claim a drop that never happened.
	//
	// Only a truncation warning may produce the truncation sentence. Other
	// warnings are real but different problems — "store(s) unavailable;
	// partial response" from a Thanos/Cortex backend or a failing remote_read
	// — and blaming those on the series limit hands the model a fabricated
	// cause and useless advice. They are passed through verbatim instead,
	// which is something it can actually act on.
	var seriesCapped bool
	var otherWarnings []string
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w), "truncated") {
			seriesCapped = true
			continue
		}
		otherWarnings = append(otherWarnings, w)
	}

	byMetric := map[string]map[string]map[string]bool{} // metric → label key → value set
	for _, set := range sets {
		name := set["__name__"]
		labels := byMetric[name]
		if labels == nil {
			labels = map[string]map[string]bool{}
			byMetric[name] = labels
		}
		for k, v := range set {
			if k == "__name__" {
				continue
			}
			if labels[k] == nil {
				labels[k] = map[string]bool{}
			}
			labels[k][v] = true
		}
	}
	names := make([]string, 0, len(byMetric))
	for name := range byMetric {
		names = append(names, name)
	}
	sort.Strings(names)
	total := len(names)
	if total > MaxMetadataMetrics {
		names = names[:MaxMetadataMetrics]
	}

	meta, err := s.prom.metadata(ctx, MaxUpstreamMetadata)
	if err != nil {
		return nil, nil, err
	}
	valuesCapped := false
	metaIncomplete := false
	metrics := make([]metricMeta, 0, len(names))
	for _, name := range names {
		m := metricMeta{Name: name, Labels: map[string][]string{}}
		if entries := meta[name]; len(entries) > 0 {
			m.Type = entries[0].Type
			m.Help = truncateString(entries[0].Help, MaxStringLen)
		}
		if m.Type == "" || m.Help == "" {
			metaIncomplete = true
		}
		for key, valueSet := range byMetric[name] {
			values := make([]string, 0, len(valueSet))
			for v := range valueSet {
				values = append(values, v)
			}
			sort.Strings(values)
			if len(values) > MaxLabelValues {
				values = values[:MaxLabelValues]
				valuesCapped = true
			}
			for i, v := range values {
				values[i] = truncateString(v, MaxStringLen)
			}
			m.Labels[key] = values
		}
		metrics = append(metrics, m)
	}

	// /api/v1/metadata does not warn when it truncates, and it fills its map
	// by walking active targets until the limit, so which metrics survive is
	// arbitrary and can differ between two calls. A full map is therefore
	// read as "possibly truncated" — but only worth saying when something in
	// the result actually lacks type or help, since that is the only reading
	// the cap can have corrupted (it used to mean "not registered", full
	// stop). Gating on it also removes the false positive on a server holding
	// exactly MaxUpstreamMetadata metrics.
	metadataCapped := len(meta) >= MaxUpstreamMetadata && metaIncomplete

	// Every cap that was applied gets its own sentence; the advice they share
	// is appended once, so a doubly-capped result does not repeat it.
	var notes []string
	metricsDropped := total > MaxMetadataMetrics
	if metricsDropped {
		notes = append(notes, fmt.Sprintf("%d metrics dropped; sorted alphabetically.", total-MaxMetadataMetrics))
	}
	if valuesCapped {
		notes = append(notes, fmt.Sprintf("Some label keys show only the first %d values (sorted).", MaxLabelValues))
	}
	if seriesCapped {
		notes = append(notes, fmt.Sprintf("Prometheus truncated the series lookup at its %d-series limit, so some metrics may be missing entirely.", MaxUpstreamSeries))
	}
	if metadataCapped {
		notes = append(notes, fmt.Sprintf("The metadata lookup hit its %d-metric limit, so an empty type or help may mean the metadata was not fetched rather than not registered.", MaxUpstreamMetadata))
	}
	if metricsDropped || seriesCapped {
		notes = append(notes, "Narrow the match selector.")
	}
	if len(otherWarnings) > 0 {
		// Verbatim, so the model sees the backend's own words, but bounded
		// like every other string this server emits.
		notes = append(notes, "Prometheus warned: "+truncateString(strings.Join(otherWarnings, "; "), MaxStringLen)+".")
	}
	payload, err := json.Marshal(metadataResult{
		Metrics: metrics,
		Truncation: metadataTruncation{
			MetricsTotal:    total,
			MetricsReturned: len(metrics),
			Note:            joinNotes(notes...),
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("serializing series_metadata result: %w", err)
	}
	return textResult(payload), nil, nil
}
