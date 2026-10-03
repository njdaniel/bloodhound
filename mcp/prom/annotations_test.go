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
	// realQuantileWarning is raised by histogram_quantile(1.5, …). It is a
	// different *kind* of warning from realBucketLabelWarning, and the whole
	// point of pickAcrossKinds: a broad selector raises one bucket-label
	// warning per metric and this one just once, so an alphabetical cap would
	// always drop it — "bucket label" sorts before "quantile value".
	realQuantileWarning = `PromQL warning: quantile value should be between 0 and 1, got 1.5 (1:20)`
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
// map and serializes them in map iteration order, so identical calls to one
// server return the same annotations differently ordered — the integration
// test measures and logs the spread on every run. Keeping "the first
// MaxQueryAnnotations" of an arbitrary order would hand the model a different
// subset every call, so the input is ordered before it is capped.
//
// Every warning here is of one kind, so the kept set is simply the
// alphabetically first; TestQueryAnnotationsKeepOneOfEachKind covers what
// happens when it is not.
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

// TestQueryAnnotationsKeepOneOfEachKind is the regression pin for the defect
// plain alphabetical capping had.
//
// The eight bucket-label repeats and the one quantile warning below are the
// exact nine a real v3.5.0 returns for `histogram_quantile(1.5, {job="…"})`
// against the integration fixture: the model wrote a percentile as a
// percentage, and Prometheus said so. But "bucket label" sorts before
// "quantile value", and the repeats outnumber the slots, so an alphabetical
// cap kept five bucket-label sentences and dropped the only one naming the
// actual bug. The model would have concluded "wrong metric type", switched
// metric, and made the same 1.5 mistake again.
//
// This is systematic rather than unlucky: Prometheus's per-metric annotations
// all begin "bucket label", and essentially every other template begins later
// in the alphabet.
func TestQueryAnnotationsKeepOneOfEachKind(t *testing.T) {
	repeats := []string{
		`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "kube_pod_container_status_ready" (1:25)`,
		`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "kube_pod_container_status_restarts_total" (1:25)`,
		`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "kube_pod_container_status_waiting_reason" (1:25)`,
		`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "scrape_duration_seconds" (1:25)`,
		`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "scrape_samples_post_metric_relabeling" (1:25)`,
		`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "scrape_samples_scraped" (1:25)`,
		`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "scrape_series_added" (1:25)`,
		`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "up" (1:25)`,
	}
	upstream := append(slices.Clone(repeats), realQuantileWarning)
	if len(repeats) <= MaxQueryAnnotations {
		t.Fatalf("fixture has %d repeats, want more than the %d cap or the crowding-out cannot happen",
			len(repeats), MaxQueryAnnotations)
	}

	// Shuffled, because upstream order is arbitrary and the guarantee must not
	// depend on the odd one out happening to arrive early.
	rng := rand.New(rand.NewSource(7))
	var first []string
	for attempt := range 10 {
		shuffled := slices.Clone(upstream)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		ann := instantAnnotations(t, shuffled, nil)
		if ann == nil {
			t.Fatal("no annotations block")
		}
		if len(ann.Warnings) != MaxQueryAnnotations {
			t.Fatalf("kept %d warnings, want the %d cap", len(ann.Warnings), MaxQueryAnnotations)
		}
		// The assertion the defect failed.
		if !slices.Contains(ann.Warnings, realQuantileWarning) {
			t.Fatalf("attempt %d dropped the one distinct warning.\nkept: %q\nmissing: %q\n"+
				"It is the only one naming the actual bug; the other %d differ only in a metric name.",
				attempt, ann.Warnings, realQuantileWarning, len(repeats))
		}
		// The repeats still get the leftover slots — one example of a kind is
		// worth much more than its second, but the second is worth more than
		// nothing.
		if got := MaxQueryAnnotations - 1; len(ann.Warnings)-1 != got {
			t.Errorf("kept %d bucket-label warnings, want the %d remaining slots", len(ann.Warnings)-1, got)
		}
		// Still deterministic and still sorted for presentation.
		if !slices.IsSorted(ann.Warnings) {
			t.Errorf("attempt %d returned warnings out of order: %q", attempt, ann.Warnings)
		}
		if attempt == 0 {
			first = ann.Warnings
			continue
		}
		if !slices.Equal(ann.Warnings, first) {
			t.Fatalf("attempt %d kept a different subset than attempt 0:\n got: %q\nwant: %q",
				attempt, ann.Warnings, first)
		}
	}
	if ann := instantAnnotations(t, upstream, nil); !strings.Contains(ann.Note, "one of each distinct kind") {
		t.Errorf("note = %q, does not describe the rule that decided the kept set", ann.Note)
	}
}

// TestAnnotationKind pins the grouping key against the real templates it has
// to separate, and against the repeats it has to merge.
func TestAnnotationKind(t *testing.T) {
	bucket := annotationKind(realBucketLabelWarning)
	other := annotationKind(`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "up" (1:25)`)
	if bucket != other {
		t.Errorf("two per-metric repeats got different kinds:\n %q\n %q", bucket, other)
	}
	quantile := annotationKind(realQuantileWarning)
	if quantile == bucket {
		t.Errorf("the quantile warning shares a kind with the bucket-label ones (%q); they must separate", bucket)
	}
	if info := annotationKind(realNotACounterInfo); info == bucket || info == quantile {
		t.Errorf("the not-a-counter info collides with another kind: %q", info)
	}
	// A message with no operand at all is its own kind rather than a panic or
	// an empty key.
	if got := annotationKind("PromQL warning: something new"); got != "PromQL warning: something new" {
		t.Errorf("operand-free message got kind %q, want the whole string", got)
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

// TestQueryAnnotationTextIsBounded checks the per-string cap. Real annotations
// run from ~132 bytes to 141 for the longest-named fixture metric, so the cap
// has to sit above MaxStringLen; what it must not do is let an unbounded
// upstream string through.
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
	// ever lowered towards MaxStringLen, where the cut lands inside the metric
	// name and removes the two parts that say which query to fix.
	for _, real := range []string{realBucketLabelWarning, realNotACounterInfo, realQuantileWarning} {
		ann = instantAnnotations(t, []string{real}, nil)
		if ann.Warnings[0] != real {
			t.Errorf("a real %d-byte annotation was truncated to %q; the cap must clear real wording",
				len(real), ann.Warnings[0])
		}
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
