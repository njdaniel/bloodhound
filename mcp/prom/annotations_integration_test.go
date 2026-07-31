//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/bloodhound/internal/promtest"
)

// rfc3339 is the timestamp layout the tools take.
const rfc3339 = time.RFC3339

// recentWindow returns a two-minute window ending now, the same shape the
// other integration tests query over.
func recentWindow() (end, start time.Time) {
	end = time.Now().UTC()
	return end, end.Add(-2 * time.Minute)
}

// rawEnvelope performs one raw GET against the Prometheus HTTP API and decodes
// the whole response envelope into out.
//
// It exists alongside promAPI (which decodes the envelope's data and returns
// only warnings) because these tests are about the envelope's own fields:
// whether an `infos` key is present, and whether a key is absent as opposed to
// present-and-empty. promAPI cannot express either question.
func rawEnvelope(t *testing.T, srv *promtest.Server, path string, params url.Values, out any) {
	t.Helper()

	u := srv.URL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("building request for %s: %v", path, err)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("calling prometheus %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading prometheus %s response: %v", path, err)
	}
	var status struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decoding prometheus %s response (HTTP %d): %v", path, resp.StatusCode, err)
	}
	if status.Status != "success" {
		t.Fatalf("prometheus %s returned status %q: %s", path, status.Status, status.Error)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decoding prometheus %s envelope: %v", path, err)
	}
}

// Upstream PromQL annotations are, like the series_metadata upstream caps, a
// claim about the *server* rather than about bloodhound's shaping. The unit
// tests in this package can only assert that mcp-prom reshapes annotations a
// fake was told to emit. What no fake can establish is what a real Prometheus
// actually emits, when, and in what form:
//
//   - that `warnings` and `infos` are two separate arrays and a single
//     response can carry both;
//   - the exact wording, which the unit fixtures claim to be copied from the
//     wire and which is passed to the model verbatim;
//   - that one expression against a broad selector produces one annotation per
//     affected metric, which is the multiplicity MaxQueryAnnotations bounds;
//   - that the endpoints mcp-prom deliberately does *not* read annotations
//     from really do not produce any.
//
// The S01 fixture is enough for all four: a gauge queried with
// histogram_quantile has no `le` label, and a gauge whose name does not end in
// _total is not a counter.

// The two annotation-provoking expressions. Both are ordinary
// incident-response moves against the wrong metric, which is the point: this
// is not an exotic corner, it is the mistake the tool exists to catch.
var (
	// bucketLabelQuery provokes a warning. The 24 characters before the
	// metric name are what puts the position at 1:25 in
	// realBucketLabelWarning.
	bucketLabelQuery = fmt.Sprintf("histogram_quantile(0.9, %s)", promtest.ReadyMetric)
	// notACounterQuery provokes an info, at position 1:6.
	notACounterQuery = fmt.Sprintf("rate(%s[1m])", promtest.ReadyMetric)
	// broadBucketLabelQuery provokes one warning per metric name in the job,
	// which is how the count cap is made to fire for real.
	broadBucketLabelQuery = fmt.Sprintf("histogram_quantile(0.9, {job=%q})", promtest.JobName)
	// mixedKindQuery provokes the same per-metric repeats *plus* one warning
	// of a different kind, by asking for a quantile of 1.5 — writing a
	// percentile as a percentage, the mistake this whole feature is meant to
	// surface. It is what makes the crowding-out pickAcrossKinds prevents
	// happen against a real server rather than only against the fake.
	mixedKindQuery = fmt.Sprintf("histogram_quantile(1.5, {job=%q})", promtest.JobName)
)

// wireAnnotations is the pair of arrays as they appear on the wire. The point
// of decoding them separately is that they *are* separate: a test that folded
// them together could not tell a warning from an info, which is the
// distinction the output shape preserves.
type wireAnnotations struct {
	Warnings []string `json:"warnings"`
	Infos    []string `json:"infos"`
}

// rawAnnotations performs one raw GET and returns just the envelope's
// annotation arrays, without decoding data.
func rawAnnotations(t *testing.T, srv *promtest.Server, path string, params url.Values) wireAnnotations {
	t.Helper()
	var env wireAnnotations
	rawEnvelope(t, srv, path, params, &env)
	return env
}

// wireToolAnnotations mirrors the annotations block of a tool payload.
type wireToolAnnotations struct {
	Warnings      []string `json:"warnings"`
	WarningsTotal int      `json:"warnings_total"`
	Infos         []string `json:"infos"`
	InfosTotal    int      `json:"infos_total"`
	Note          string   `json:"note"`
}

// wireAnnotatedResult mirrors just the annotations block of any tool payload.
type wireAnnotatedResult struct {
	Annotations *wireToolAnnotations `json:"annotations"`
}

// TestUpstreamAnnotationsAgainstRealPrometheus drives one container through
// both halves: the wire half pins what Prometheus v3.5.0 emits, and the tool
// half checks the real mcp-prom binary delivers it to the model.
func TestUpstreamAnnotationsAgainstRealPrometheus(t *testing.T) {
	_, srv := startStack(t)

	t.Run("wire", func(t *testing.T) { annotationWireBehaviour(t, srv) })
	t.Run("silent endpoints", func(t *testing.T) { annotationSilentEndpoints(t, srv) })
	t.Run("tool", func(t *testing.T) { annotationToolReport(t, srv) })
	t.Run("cap", func(t *testing.T) { annotationToolCap(t, srv) })
	t.Run("mixed kinds", func(t *testing.T) { annotationToolMixedKinds(t, srv) })
}

// annotationToolMixedKinds is the end-to-end regression pin for the defect
// plain alphabetical capping had: when per-metric repeats overflow the cap and
// a distinct warning co-occurs, the distinct one is the one that must survive.
//
// Both halves are asserted against the real server — that it really does raise
// the two kinds together (a fake could be made to say anything), and that the
// kept five include the odd one out.
func annotationToolMixedKinds(t *testing.T, srv *promtest.Server) {
	// Not t.Helper(), for the same reason as annotationWireBehaviour.

	// Wire first: the fixture must actually produce the situation, or the tool
	// assertion below would pass vacuously.
	wire := rawAnnotations(t, srv, "/api/v1/query", url.Values{"query": {mixedKindQuery}})
	var repeats, distinct []string
	for _, w := range wire.Warnings {
		if strings.Contains(w, "bucket label") {
			repeats = append(repeats, w)
			continue
		}
		distinct = append(distinct, w)
	}
	if len(repeats) <= MaxQueryAnnotations {
		t.Fatalf("%q raised %d per-metric repeats, want more than the %d cap or nothing can be crowded out: %q",
			mixedKindQuery, len(repeats), MaxQueryAnnotations, wire.Warnings)
	}
	if len(distinct) != 1 {
		t.Fatalf("%q raised %d warnings of a kind other than bucket-label, want exactly the quantile one: %q",
			mixedKindQuery, len(distinct), distinct)
	}
	if distinct[0] != realQuantileWarning {
		t.Errorf("real quantile warning is %q, want %q. Finding, not fixup: the unit test "+
			"TestQueryAnnotationsKeepOneOfEachKind uses that constant as its fixture.",
			distinct[0], realQuantileWarning)
	}
	// The bias is only a bias because of where the two kinds sort. Spell that
	// out here, so a future reader can see why the round-robin exists without
	// re-deriving it.
	if distinct[0] <= repeats[0] {
		t.Errorf("the distinct warning %q no longer sorts after the repeats %q; "+
			"alphabetical capping would not have dropped it and this test no longer pins what it claims",
			distinct[0], repeats[0])
	}

	// Then the tool: the distinct warning survives the cap.
	session := connect(t, srv.URL)
	var got wireAnnotatedResult
	callTool(t, session, "query_instant", map[string]any{"query": mixedKindQuery}, &got)
	if got.Annotations == nil {
		t.Fatalf("no annotations block for %q", mixedKindQuery)
	}
	if len(got.Annotations.Warnings) != MaxQueryAnnotations {
		t.Fatalf("kept %d warnings, want the %d cap: %q",
			len(got.Annotations.Warnings), MaxQueryAnnotations, got.Annotations.Warnings)
	}
	if !slices.Contains(got.Annotations.Warnings, realQuantileWarning) {
		t.Errorf("the capped list dropped the one distinct warning.\nkept: %q\nmissing: %q\n"+
			"A model told only the bucket-label repeats would fix the metric type and repeat the "+
			"1.5-should-be-0.95 mistake on its next query.",
			got.Annotations.Warnings, realQuantileWarning)
	}
	t.Logf("%d upstream warnings (%d per-metric repeats + %d distinct) capped to %d, distinct one kept",
		got.Annotations.WarningsTotal, len(repeats), len(distinct), len(got.Annotations.Warnings))
}

// annotationWireBehaviour pins the server behaviour the output shape is built
// on: two severities in two arrays, the exact wording the unit fixtures copy,
// and one annotation per affected metric.
func annotationWireBehaviour(t *testing.T, srv *promtest.Server) {
	// Deliberately not t.Helper(): this function is the subtest, not an
	// assertion helper, and marking it collapses every failure onto the t.Run
	// line instead of the assertion that fired.

	// The unit fixtures name a metric; if the fixture were renamed out from
	// under them the exact-wording assertions below would fail for a reason
	// that has nothing to do with Prometheus.
	if !strings.Contains(realBucketLabelWarning, promtest.ReadyMetric) {
		t.Fatalf("realBucketLabelWarning %q does not name the fixture metric %q; the unit fixture and this test have drifted apart",
			realBucketLabelWarning, promtest.ReadyMetric)
	}

	// Control: a well-formed query against the same data is silent. Without
	// this, an annotation below could be ambient rather than caused by the
	// expression.
	if ann := rawAnnotations(t, srv, "/api/v1/query", url.Values{"query": {promtest.ReadyMetric}}); len(ann.Warnings)+len(ann.Infos) != 0 {
		t.Fatalf("a plain instant query warned %v / informed %v; the fixture is meant to be quiet until the expression provokes it", ann.Warnings, ann.Infos)
	}

	// A warning, on both query endpoints. Exact text first: this is what says
	// the wording moved.
	end, start := recentWindow()
	for _, ep := range []struct {
		path   string
		params url.Values
	}{
		{"/api/v1/query", url.Values{"query": {bucketLabelQuery}}},
		{"/api/v1/query_range", url.Values{
			"query": {bucketLabelQuery}, "start": {formatTime(start)}, "end": {formatTime(end)}, "step": {"15"}}},
	} {
		ann := rawAnnotations(t, srv, ep.path, ep.params)
		if len(ann.Warnings) != 1 {
			t.Fatalf("%s %q produced %d warnings %q, want exactly one", ep.path, bucketLabelQuery, len(ann.Warnings), ann.Warnings)
		}
		if ann.Warnings[0] != realBucketLabelWarning {
			t.Errorf("real %s warning is %q, want %q.\n"+
				"This is a finding, not a test fixup: the unit tests in annotations_test.go use that constant as their "+
				"fixture on the claim that it is what the wire carries, and mcp-prom passes it to the model verbatim. "+
				"Update both together, and re-read the new wording for anything the model now needs told.",
				ep.path, ann.Warnings[0], realBucketLabelWarning)
		}
		if len(ann.Infos) != 0 {
			t.Errorf("%s %q also produced infos %q; the severities are meant to be independent", ep.path, bucketLabelQuery, ann.Infos)
		}
	}

	// An info, on both query endpoints. This is the annotation that matters
	// most here: "name does not end in _total" is the PromQL mistake a model
	// makes most often, and it arrives at a severity a warnings-only
	// implementation would never see.
	for _, ep := range []struct {
		path   string
		params url.Values
	}{
		{"/api/v1/query", url.Values{"query": {notACounterQuery}}},
		{"/api/v1/query_range", url.Values{
			"query": {notACounterQuery}, "start": {formatTime(start)}, "end": {formatTime(end)}, "step": {"15"}}},
	} {
		ann := rawAnnotations(t, srv, ep.path, ep.params)
		if len(ann.Infos) != 1 {
			t.Fatalf("%s %q produced %d infos %q, want exactly one", ep.path, notACounterQuery, len(ann.Infos), ann.Infos)
		}
		if ann.Infos[0] != realNotACounterInfo {
			t.Errorf("real %s info is %q, want %q. Same finding-not-fixup note as the warning above.",
				ep.path, ann.Infos[0], realNotACounterInfo)
		}
		if len(ann.Warnings) != 0 {
			t.Errorf("%s %q also produced warnings %q; the severities are meant to be independent", ep.path, notACounterQuery, ann.Warnings)
		}
	}

	// Both at once, in separate arrays. This is the assertion that would fail
	// if the two ever merged upstream, which is the premise of keeping them
	// apart in the output.
	both := rawAnnotations(t, srv, "/api/v1/query", url.Values{
		"query": {notACounterQuery + " + " + bucketLabelQuery}})
	if len(both.Warnings) != 1 || len(both.Infos) != 1 {
		t.Errorf("an expression provoking both produced %d warnings %q and %d infos %q, want one of each in separate arrays",
			len(both.Warnings), both.Warnings, len(both.Infos), both.Infos)
	}

	// One annotation per affected metric: the reason MaxQueryAnnotations
	// exists at all. Three fixture metrics plus the per-scrape metrics
	// Prometheus synthesises is comfortably past the cap, but the exact count
	// is a property of the server version, so only the inequality is asserted.
	broad := rawAnnotations(t, srv, "/api/v1/query", url.Values{"query": {broadBucketLabelQuery}})
	if len(broad.Warnings) <= MaxQueryAnnotations {
		t.Errorf("a selector spanning the whole job produced %d warnings, want more than the %d cap or the cap cannot be exercised: %q",
			len(broad.Warnings), MaxQueryAnnotations, broad.Warnings)
	}
	for _, w := range broad.Warnings {
		if len(w) > MaxQueryAnnotationLen {
			t.Errorf("a real warning is %d bytes, past the %d-byte cap, so the model would see it truncated: %q",
				len(w), MaxQueryAnnotationLen, w)
		}
	}

	// Order is upstream map iteration order, which is why shapeAnnotations
	// sorts before capping. Asserting that it *varies* would be flaky in the
	// other direction, so the variation is only recorded; what the tool must
	// do about it is asserted in the cap subtest.
	orderings := map[string]int{}
	for range 20 {
		got := rawAnnotations(t, srv, "/api/v1/query", url.Values{"query": {broadBucketLabelQuery}})
		orderings[strings.Join(got.Warnings, "|")]++
	}
	t.Logf("20 identical /api/v1/query calls returned the same %d warnings in %d distinct orders; "+
		"capping without sorting would hand the model a different subset per call",
		len(broad.Warnings), len(orderings))
}

// annotationSilentEndpoints pins the scope decision: /api/v1/alerts and
// /api/v1/metadata do not evaluate PromQL, so they have no annotations to
// attach and promClient.get may keep discarding them. If this ever fails,
// those two tools need the same treatment query_range and query_instant got.
func annotationSilentEndpoints(t *testing.T, srv *promtest.Server) {
	// Not t.Helper(), for the same reason as annotationWireBehaviour.
	for _, path := range []string{"/api/v1/alerts", "/api/v1/metadata"} {
		// Decoded as raw messages rather than []string so that an emitted but
		// empty array is distinguishable from an absent key: the claim is that
		// these endpoints do not carry the fields at all.
		var env struct {
			Warnings json.RawMessage `json:"warnings"`
			Infos    json.RawMessage `json:"infos"`
		}
		rawEnvelope(t, srv, path, nil, &env)
		if env.Warnings != nil || env.Infos != nil {
			t.Errorf("%s carried warnings=%s infos=%s. promClient.get discards annotations on the premise that "+
				"endpoints which never evaluate PromQL never produce them — if that is no longer true, list_alerts "+
				"and series_metadata need an annotations block too.", path, env.Warnings, env.Infos)
		}
	}
}

// annotationToolReport drives the real mcp-prom binary and checks both
// severities reach the model verbatim on both query tools. This is the
// end-to-end form of the bug: before the fix every payload below carried no
// annotations block at all.
func annotationToolReport(t *testing.T, srv *promtest.Server) {
	// Not t.Helper(), for the same reason as annotationWireBehaviour.
	session := connect(t, srv.URL)
	end, start := recentWindow()

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"query_instant", map[string]any{"query": bucketLabelQuery}},
		{"query_range", map[string]any{
			"query": bucketLabelQuery, "start": start.Format(rfc3339), "end": end.Format(rfc3339)}},
	} {
		var got wireAnnotatedResult
		callTool(t, session, tc.name, tc.args, &got)
		if got.Annotations == nil {
			t.Errorf("%s returned no annotations block for %q; Prometheus warned that the result is meaningless "+
				"and the model was not told", tc.name, bucketLabelQuery)
			continue
		}
		if !slices.Equal(got.Annotations.Warnings, []string{realBucketLabelWarning}) {
			t.Errorf("%s warnings = %q, want the server's own wording unchanged", tc.name, got.Annotations.Warnings)
		}
		if got.Annotations.WarningsTotal != 1 {
			t.Errorf("%s warnings_total = %d, want 1", tc.name, got.Annotations.WarningsTotal)
		}
		if len(got.Annotations.Infos) != 0 || got.Annotations.InfosTotal != 0 {
			t.Errorf("%s reported infos %q for a warning-only query", tc.name, got.Annotations.Infos)
		}
		if got.Annotations.Note != "" {
			t.Errorf("%s annotation note = %q, want empty when nothing was capped", tc.name, got.Annotations.Note)
		}
	}

	// The info half, end to end.
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"query_instant", map[string]any{"query": notACounterQuery}},
		{"query_range", map[string]any{
			"query": notACounterQuery, "start": start.Format(rfc3339), "end": end.Format(rfc3339)}},
	} {
		var got wireAnnotatedResult
		callTool(t, session, tc.name, tc.args, &got)
		if got.Annotations == nil {
			t.Errorf("%s returned no annotations block for %q; the not-a-counter info is the one this project "+
				"most needs to reach the model", tc.name, notACounterQuery)
			continue
		}
		if !slices.Equal(got.Annotations.Infos, []string{realNotACounterInfo}) {
			t.Errorf("%s infos = %q, want the server's own wording unchanged", tc.name, got.Annotations.Infos)
		}
		if len(got.Annotations.Warnings) != 0 {
			t.Errorf("%s reported warnings %q for an info-only query", tc.name, got.Annotations.Warnings)
		}
	}

	// And a well-formed query carries no block at all, so its absence is a
	// usable signal rather than a default the model has to check.
	var quiet wireAnnotatedResult
	callTool(t, session, "query_instant", map[string]any{"query": promtest.ReadyMetric}, &quiet)
	if quiet.Annotations != nil {
		t.Errorf("a well-formed query carried an annotations block %+v; absence is meant to mean Prometheus was quiet", quiet.Annotations)
	}
}

// annotationToolCap makes the count cap fire against the real server and
// checks the two properties that make a capped list usable: it is marked, and
// it is the same list every call.
func annotationToolCap(t *testing.T, srv *promtest.Server) {
	// Not t.Helper(), for the same reason as annotationWireBehaviour.
	session := connect(t, srv.URL)

	var first []string
	var total int
	const calls = 5
	for i := range calls {
		var got wireAnnotatedResult
		callTool(t, session, "query_instant", map[string]any{"query": broadBucketLabelQuery}, &got)
		if got.Annotations == nil {
			t.Fatalf("call %d returned no annotations block for %q", i, broadBucketLabelQuery)
		}
		if len(got.Annotations.Warnings) != MaxQueryAnnotations {
			t.Fatalf("call %d returned %d warnings, want the %d cap: %q",
				i, len(got.Annotations.Warnings), MaxQueryAnnotations, got.Annotations.Warnings)
		}
		if got.Annotations.WarningsTotal <= MaxQueryAnnotations {
			t.Fatalf("call %d reported warnings_total = %d, want the full upstream count above the cap",
				i, got.Annotations.WarningsTotal)
		}
		// Marked the way every other cap in this package is marked.
		want := fmt.Sprintf("%d further warnings dropped", got.Annotations.WarningsTotal-MaxQueryAnnotations)
		if !strings.Contains(got.Annotations.Note, want) {
			t.Errorf("call %d note = %q, does not contain %q", i, got.Annotations.Note, want)
		}
		if !slices.IsSorted(got.Annotations.Warnings) {
			t.Errorf("call %d returned warnings out of order: %q", i, got.Annotations.Warnings)
		}

		if i == 0 {
			first, total = got.Annotations.Warnings, got.Annotations.WarningsTotal
			continue
		}
		// The determinism assertion. Prometheus serializes these in map
		// iteration order, so keeping the first MaxQueryAnnotations of what
		// arrived would return a different subset per call — with %d of %d
		// kept, five matching draws by luck is vanishingly unlikely.
		if !slices.Equal(got.Annotations.Warnings, first) {
			t.Fatalf("call %d kept a different subset of the %d upstream warnings than call 0:\n got: %q\nwant: %q\n"+
				"shapeAnnotations must sort before capping — upstream order is not stable.",
				i, total, got.Annotations.Warnings, first)
		}
	}
	t.Logf("%d identical query_instant calls each kept the same %d of %d upstream warnings", calls, MaxQueryAnnotations, total)
}
