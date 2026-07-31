package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// The PromQL annotations these tests feed the fake are the exact strings a
// Prometheus v3.5.0 produced against the integration fixture, position suffix
// and all. They are copied rather than paraphrased so the unit tests and the
// integration test are reasoning about the same bytes; the integration test is
// what proves they are still what the server sends.
const (
	realBucketLabelWarning = `PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "kube_pod_container_status_ready" (1:25)`
	realNotACounterInfo    = `PromQL info: metric might not be a counter, name does not end in _total/_sum/_count/_bucket: "kube_pod_container_status_ready" (1:6)`
)

// annotatedBody adds warnings and infos to an already-built success envelope,
// mirroring how Prometheus attaches them alongside data.
func annotatedBody(t *testing.T, base string, warnings, infos []string) string {
	t.Helper()
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(base), &env); err != nil {
		t.Fatalf("re-parsing fixture envelope: %v", err)
	}
	add := func(key string, vals []string) {
		if len(vals) == 0 {
			return
		}
		raw, err := json.Marshal(vals)
		if err != nil {
			t.Fatalf("marshaling %s fixture: %v", key, err)
		}
		env[key] = raw
	}
	add("warnings", warnings)
	add("infos", infos)
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("re-marshaling fixture envelope: %v", err)
	}
	return string(body)
}

// gotAnnotations mirrors the annotations block for assertions. It is a pointer
// in the enclosing structs so a test can tell "block absent" from "block
// present but empty" — the distinction the omitempty tag exists to make.
type gotAnnotations struct {
	Warnings      []string `json:"warnings"`
	WarningsTotal int      `json:"warnings_total"`
	Infos         []string `json:"infos"`
	InfosTotal    int      `json:"infos_total"`
	Note          string   `json:"note"`
}

// gotAnnotated decodes only the annotations block out of any tool payload.
type gotAnnotated struct {
	Annotations *gotAnnotations `json:"annotations"`
}

// instantAnnotations runs query_instant against a fake whose /api/v1/query
// carries the given annotations and returns the decoded block.
func instantAnnotations(t *testing.T, warnings, infos []string) *gotAnnotations {
	t.Helper()
	fake := newFakeProm(t)
	fake.set("/api/v1/query", 200, annotatedBody(t, vectorJSON(t, []map[string]any{
		sampleFixture(map[string]string{"pod": "checkout-7d9f"}, 1753700000, "0"),
	}), warnings, infos))
	res, _, err := newTestToolServer(fake).handleQueryInstant(context.Background(), nil, queryInstantInput{Query: "q"})
	if err != nil {
		t.Fatalf("handleQueryInstant: %v", err)
	}
	var g gotAnnotated
	if err := json.Unmarshal([]byte(resultText(t, res)), &g); err != nil {
		t.Fatalf("decoding query_instant payload: %v", err)
	}
	return g.Annotations
}

// rangeAnnotations is instantAnnotations for query_range.
func rangeAnnotations(t *testing.T, warnings, infos []string) *gotAnnotations {
	t.Helper()
	fake := newFakeProm(t)
	fake.set("/api/v1/query_range", 200, annotatedBody(t, matrixJSON(t, []map[string]any{
		seriesFixture(map[string]string{"pod": "checkout-7d9f"}, [][2]any{{1753696800, "0"}, {1753696830, "1"}}),
	}), warnings, infos))
	res, _, err := newTestToolServer(fake).handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "q", Start: "2026-07-28T10:00:00Z", End: "2026-07-28T11:00:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryRange: %v", err)
	}
	var g gotAnnotated
	if err := json.Unmarshal([]byte(resultText(t, res)), &g); err != nil {
		t.Fatalf("decoding query_range payload: %v", err)
	}
	return g.Annotations
}

// TestQueryToolsSurfaceBothAnnotationSeverities is the issue's core claim: the
// two tools that evaluate PromQL used to drop everything Prometheus said about
// the evaluation. Both severities must arrive, verbatim, and in separate
// arrays.
func TestQueryToolsSurfaceBothAnnotationSeverities(t *testing.T) {
	for _, tc := range []struct {
		tool string
		run  func(*testing.T, []string, []string) *gotAnnotations
	}{
		{"query_instant", instantAnnotations},
		{"query_range", rangeAnnotations},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			ann := tc.run(t, []string{realBucketLabelWarning}, []string{realNotACounterInfo})
			if ann == nil {
				t.Fatal("no annotations block; the upstream annotations were dropped")
			}
			// Verbatim: the metric name and the (line:col) position are the
			// only parts that say which query to fix, so any reshaping that
			// loses them defeats the point.
			if !slices.Equal(ann.Warnings, []string{realBucketLabelWarning}) {
				t.Errorf("warnings = %q, want the upstream warning unchanged", ann.Warnings)
			}
			if !slices.Equal(ann.Infos, []string{realNotACounterInfo}) {
				t.Errorf("infos = %q, want the upstream info unchanged", ann.Infos)
			}
			// Separate arrays, not one flattened list: the severities call for
			// different actions and the wire format distinguishes them.
			if ann.WarningsTotal != 1 || ann.InfosTotal != 1 {
				t.Errorf("totals = %d warnings / %d infos, want 1 and 1", ann.WarningsTotal, ann.InfosTotal)
			}
			if ann.Note != "" {
				t.Errorf("note = %q, want empty when nothing was capped", ann.Note)
			}
		})
	}
}

// TestQueryToolsOmitAnnotationsWhenServerIsQuiet pins the "absence is the
// signal" half of the shape. It asserts on the raw JSON rather than a decoded
// struct because the claim is about the key not being emitted at all — a
// decode into a pointer would report the same nil for an emitted `null`.
func TestQueryToolsOmitAnnotationsWhenServerIsQuiet(t *testing.T) {
	fake := goldenFake(t)
	ts := newTestToolServer(fake)

	instant, _, err := ts.handleQueryInstant(context.Background(), nil, queryInstantInput{Query: "q"})
	if err != nil {
		t.Fatalf("handleQueryInstant: %v", err)
	}
	rng, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "q", Start: "2026-07-28T10:00:00Z", End: "2026-07-28T11:00:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryRange: %v", err)
	}
	for _, payload := range []string{resultText(t, instant), resultText(t, rng)} {
		if strings.Contains(payload, "annotations") {
			t.Errorf("payload names annotations though the server raised none: %s", payload)
		}
	}
}

// TestAlertsAndMetadataToolsCarryNoAnnotationsBlock pins the scope decision.
// /api/v1/alerts and /api/v1/metadata do not evaluate PromQL, so they have no
// annotations to carry and the two tools built on them gain no block. The
// integration suite pins the upstream half of that claim; this pins that
// bloodhound did not grow a block anyway.
func TestAlertsAndMetadataToolsCarryNoAnnotationsBlock(t *testing.T) {
	ts := newTestToolServer(goldenFake(t))

	alerts, _, err := ts.handleListAlerts(context.Background(), nil, listAlertsInput{})
	if err != nil {
		t.Fatalf("handleListAlerts: %v", err)
	}
	meta, _, err := ts.handleSeriesMetadata(context.Background(), nil, seriesMetadataInput{Match: `{namespace="shop"}`})
	if err != nil {
		t.Fatalf("handleSeriesMetadata: %v", err)
	}
	for _, payload := range []string{resultText(t, alerts), resultText(t, meta)} {
		var g gotAnnotated
		if err := json.Unmarshal([]byte(payload), &g); err != nil {
			t.Fatalf("decoding payload: %v", err)
		}
		if g.Annotations != nil {
			t.Errorf("payload carries an annotations block: %s", payload)
		}
	}
}

// bulkWarnings builds n distinct annotations shaped like the real one, which
// differs per metric only in the metric name — the multiplicity the cap exists
// for.
func bulkWarnings(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf(
			`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "metric_%02d" (1:25)`, i))
	}
	return out
}

// TestQueryAnnotationsAreCappedAndDeterministic covers the bound and the
// property that makes the bound safe. Prometheus accumulates annotations in a
// map, so it serializes them in map iteration order — 30 identical calls to one
// v3.5.0 server returned the same eight warnings in eight different orders.
// Keeping "the first MaxQueryAnnotations" of an arbitrary order would hand the
// model a different subset every call, so the cap sorts first.
func TestQueryAnnotationsAreCappedAndDeterministic(t *testing.T) {
	const total = MaxQueryAnnotations + 7
	want := bulkWarnings(total)[:MaxQueryAnnotations] // bulkWarnings is already sorted

	// Ten different upstream orderings of one set must all produce the same
	// kept subset, in the same order.
	rng := rand.New(rand.NewSource(1))
	for attempt := range 10 {
		shuffled := bulkWarnings(total)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		ann := instantAnnotations(t, shuffled, nil)
		if ann == nil {
			t.Fatal("no annotations block")
		}
		if !slices.Equal(ann.Warnings, want) {
			t.Fatalf("attempt %d kept %q, want the alphabetically first %d regardless of upstream order: %q",
				attempt, ann.Warnings, MaxQueryAnnotations, want)
		}
		if ann.WarningsTotal != total {
			t.Fatalf("warnings_total = %d, want the full %d the server sent", ann.WarningsTotal, total)
		}
		// Marked the way every other cap in this package is marked: a
		// sentence naming the count dropped, plus the shared advice once.
		if !strings.Contains(ann.Note, fmt.Sprintf("%d further warnings dropped", total-MaxQueryAnnotations)) {
			t.Errorf("note = %q, does not report the %d dropped warnings", ann.Note, total-MaxQueryAnnotations)
		}
		if !strings.Contains(ann.Note, "narrow it") {
			t.Errorf("note = %q, does not tell the model what to do about it", ann.Note)
		}
	}
}

// TestQueryAnnotationsCapEachSeverityIndependently checks the caps do not
// share a budget: a flood of warnings must not crowd out the single info,
// which is the annotation this project cares about most.
func TestQueryAnnotationsCapEachSeverityIndependently(t *testing.T) {
	const total = MaxQueryAnnotations + 3
	ann := instantAnnotations(t, bulkWarnings(total), []string{realNotACounterInfo})
	if ann == nil {
		t.Fatal("no annotations block")
	}
	if len(ann.Warnings) != MaxQueryAnnotations {
		t.Errorf("kept %d warnings, want the %d cap", len(ann.Warnings), MaxQueryAnnotations)
	}
	if !slices.Equal(ann.Infos, []string{realNotACounterInfo}) {
		t.Errorf("infos = %q, want the info to survive a flood of warnings", ann.Infos)
	}
	if ann.InfosTotal != 1 {
		t.Errorf("infos_total = %d, want 1", ann.InfosTotal)
	}
	if strings.Contains(ann.Note, "infos dropped") {
		t.Errorf("note = %q claims infos were dropped; only warnings were", ann.Note)
	}
}

// TestQueryAnnotationTextIsBounded checks the per-string cap. The real
// annotations run to 141 bytes, so the cap has to sit above MaxStringLen; what
// it must not do is let an unbounded upstream string through.
func TestQueryAnnotationTextIsBounded(t *testing.T) {
	long := "PromQL warning: " + strings.Repeat("x", 4000)
	ann := instantAnnotations(t, []string{long}, nil)
	if ann == nil {
		t.Fatal("no annotations block")
	}
	got := ann.Warnings[0]
	if len(got) > MaxQueryAnnotationLen {
		t.Errorf("warning is %d bytes, want at most the %d cap", len(got), MaxQueryAnnotationLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated warning %q does not carry the ellipsis marker every other cap in this package uses", got)
	}
	// The real wording must survive intact, or the cap is set too low to be
	// useful — this is the assertion that fails if MaxQueryAnnotationLen is
	// ever lowered towards MaxStringLen.
	ann = instantAnnotations(t, []string{realBucketLabelWarning}, nil)
	if ann.Warnings[0] != realBucketLabelWarning {
		t.Errorf("the real 141-byte warning was truncated to %q; the cap must clear real wording", ann.Warnings[0])
	}
}

// TestQueryAnnotationsDeduplicate checks the cap spends its slots on distinct
// diagnostics. Prometheus keys annotations by message so the wire arrays are
// already distinct; this is what keeps that from being load-bearing.
func TestQueryAnnotationsDeduplicate(t *testing.T) {
	dupes := make([]string, MaxQueryAnnotations+5)
	for i := range dupes {
		dupes[i] = realBucketLabelWarning
	}
	ann := instantAnnotations(t, dupes, nil)
	if ann == nil {
		t.Fatal("no annotations block")
	}
	if !slices.Equal(ann.Warnings, []string{realBucketLabelWarning}) {
		t.Errorf("warnings = %q, want one entry after de-duplication", ann.Warnings)
	}
	if ann.WarningsTotal != 1 {
		t.Errorf("warnings_total = %d, want the distinct count", ann.WarningsTotal)
	}
	if ann.Note != "" {
		t.Errorf("note = %q, want empty: nothing was dropped once the repeats collapsed", ann.Note)
	}
}

// TestQueryRangeAnnotationsSurviveTheSizeBackstop pins the priority the
// backstop must respect. Point-thinning exists to fit the response cap; if it
// could reach the annotations block it would be dropping "this result may be
// meaningless" to make room for the meaningless numbers.
func TestQueryRangeAnnotationsSurviveTheSizeBackstop(t *testing.T) {
	// Enough series and points to blow well past MaxResponseBytes and force
	// several thinning passes.
	series := make([]map[string]any, 0, MaxSeries)
	for s := range MaxSeries {
		values := make([][2]any, 0, 120)
		for i := range 120 {
			values = append(values, [2]any{1753696800 + i*30, fmt.Sprintf("%d.%d", s, i)})
		}
		series = append(series, seriesFixture(
			map[string]string{"pod": fmt.Sprintf("pod-%02d", s), "namespace": "shop"}, values))
	}
	fake := newFakeProm(t)
	fake.set("/api/v1/query_range", 200, annotatedBody(t, matrixJSON(t, series),
		[]string{realBucketLabelWarning}, []string{realNotACounterInfo}))

	res, _, err := newTestToolServer(fake).handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: "q", Start: "2026-07-28T10:00:00Z", End: "2026-07-28T11:00:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryRange: %v", err)
	}
	payload := resultText(t, res)

	var g struct {
		Truncation struct {
			PointsThinned bool `json:"points_thinned"`
		} `json:"truncation"`
		Annotations *gotAnnotations `json:"annotations"`
	}
	if err := json.Unmarshal([]byte(payload), &g); err != nil {
		t.Fatalf("decoding query_range payload: %v", err)
	}
	if !g.Truncation.PointsThinned {
		t.Fatalf("fixture did not trigger the size backstop (%d bytes); it cannot test surviving it", len(payload))
	}
	if g.Annotations == nil {
		t.Fatal("the size backstop dropped the annotations block")
	}
	if !slices.Equal(g.Annotations.Warnings, []string{realBucketLabelWarning}) {
		t.Errorf("warnings = %q after thinning, want them intact", g.Annotations.Warnings)
	}
	if !slices.Equal(g.Annotations.Infos, []string{realNotACounterInfo}) {
		t.Errorf("infos = %q after thinning, want them intact", g.Annotations.Infos)
	}
}

// TestQueryDescriptionsQuoteTheAnnotationCap keeps the model-facing text
// honest about the number it promises, the same guard
// TestSeriesMetadataDescriptionQuotesLookback gives MetadataLookback.
func TestQueryDescriptionsQuoteTheAnnotationCap(t *testing.T) {
	want := fmt.Sprintf("At most %d of each", MaxQueryAnnotations)
	for name, desc := range map[string]string{
		"query_range":   queryRangeDescription,
		"query_instant": queryInstantDescription,
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("%s description does not quote MaxQueryAnnotations (%q):\n%s", name, want, desc)
		}
		if !strings.Contains(desc, "annotations block") {
			t.Errorf("%s description never tells the model the annotations block exists:\n%s", name, desc)
		}
	}
}
