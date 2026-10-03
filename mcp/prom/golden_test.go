package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden rewrites the golden files instead of comparing against them:
//
//	go test ./mcp/prom/ -run TestGolden -update
var updateGolden = flag.Bool("update", false, "rewrite golden files")

// checkGolden compares got against testdata/<name>.golden byte-exact.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("creating testdata dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("result differs from golden %s (byte-exact):\n got: %s\nwant: %s", path, got, want)
	}
}

// goldenFake serves the fixed fixtures behind every golden test.
func goldenFake(t *testing.T) *fakeProm {
	t.Helper()
	fake := newFakeProm(t)
	fake.set("/api/v1/query_range", 200, matrixJSON(t, []map[string]any{
		seriesFixture(map[string]string{"pod": "checkout-7d9f", "namespace": "shop"},
			[][2]any{{1753696800, "0.1"}, {1753696830, "42.7"}, {1753696860, "41.9"}}),
		seriesFixture(map[string]string{"pod": "payments-5c4b", "namespace": "shop"},
			[][2]any{{1753696800, "3.0"}, {1753696830, "3.5"}, {1753696860, "3.14159"}}),
	}))
	fake.set("/api/v1/query", 200, vectorJSON(t, []map[string]any{
		sampleFixture(map[string]string{"pod": "checkout-7d9f", "namespace": "shop"}, 1753700000, "42.7"),
		sampleFixture(map[string]string{"pod": "payments-5c4b", "namespace": "shop"}, 1753700000, "3.14159"),
	}))
	fake.set("/api/v1/alerts", 200, alertsJSON(t, []map[string]any{
		alertFixture("HighErrorRate", "firing", "2026-07-28T09:45:00Z",
			map[string]string{"summary": "Error ratio above 5% for 10m"}),
		alertFixture("PodOOMKilled", "firing", "2026-07-28T10:12:00Z",
			map[string]string{"summary": "checkout pod OOMKilled twice"}),
		alertFixture("SlowBurn", "pending", "2026-07-28T10:20:00Z",
			map[string]string{"summary": "not firing yet"}),
	}))
	fake.set("/api/v1/series", 200, seriesJSON(t, []map[string]string{
		{"__name__": "http_requests_total", "pod": "checkout-7d9f", "namespace": "shop"},
		{"__name__": "http_requests_total", "pod": "payments-5c4b", "namespace": "shop"},
		{"__name__": "container_memory_usage_bytes", "pod": "checkout-7d9f"},
	}))
	fake.set("/api/v1/metadata", 200, `{"status":"success","data":{`+
		`"http_requests_total":[{"type":"counter","help":"Total HTTP requests by pod.","unit":""}],`+
		`"container_memory_usage_bytes":[{"type":"gauge","help":"Container memory usage in bytes.","unit":""}]}}`)
	return fake
}

func TestGoldenQueryRange(t *testing.T) {
	ts := newTestToolServer(goldenFake(t))
	res, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: `rate(http_requests_total{namespace="shop"}[5m])`,
		Start: "2026-07-28T10:00:00Z",
		End:   "2026-07-28T11:00:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryRange: %v", err)
	}
	checkGolden(t, "query_range", resultText(t, res))
}

func TestGoldenQueryInstant(t *testing.T) {
	ts := newTestToolServer(goldenFake(t))
	res, _, err := ts.handleQueryInstant(context.Background(), nil, queryInstantInput{
		Query: `rate(http_requests_total{namespace="shop"}[5m])`,
		Time:  "2026-07-28T10:15:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryInstant: %v", err)
	}
	checkGolden(t, "query_instant", resultText(t, res))
}

func TestGoldenListAlerts(t *testing.T) {
	ts := newTestToolServer(goldenFake(t))
	res, _, err := ts.handleListAlerts(context.Background(), nil, listAlertsInput{})
	if err != nil {
		t.Fatalf("handleListAlerts: %v", err)
	}
	checkGolden(t, "list_alerts", resultText(t, res))
}

// annotatedGoldenFake is goldenFake with the two PromQL annotations a real
// v3.5.0 raises attached to both query endpoints, plus a third warning so the
// arrays are not single-element.
//
// It is a separate fixture rather than an edit to goldenFake on purpose: the
// four original goldens stay byte-identical, which is the evidence that adding
// the annotations block changed nothing for a response Prometheus said nothing
// about. It also means those four never reached this code path before — a
// quiet server is all they ever simulated — so the new shape needs goldens of
// its own or it would ship with no byte-exact coverage at all.
func annotatedGoldenFake(t *testing.T) *fakeProm {
	t.Helper()
	warnings := []string{
		realBucketLabelWarning,
		`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name "up" (1:25)`,
	}
	infos := []string{realNotACounterInfo}

	fake := goldenFake(t)
	for _, path := range []string{"/api/v1/query", "/api/v1/query_range"} {
		fake.set(path, 200, annotatedBody(t, fake.body(path), warnings, infos))
	}
	return fake
}

func TestGoldenQueryRangeAnnotated(t *testing.T) {
	ts := newTestToolServer(annotatedGoldenFake(t))
	res, _, err := ts.handleQueryRange(context.Background(), nil, queryRangeInput{
		Query: `histogram_quantile(0.9, rate(http_requests_total{namespace="shop"}[5m]))`,
		Start: "2026-07-28T10:00:00Z",
		End:   "2026-07-28T11:00:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryRange: %v", err)
	}
	checkGolden(t, "query_range_annotated", resultText(t, res))
}

func TestGoldenQueryInstantAnnotated(t *testing.T) {
	ts := newTestToolServer(annotatedGoldenFake(t))
	res, _, err := ts.handleQueryInstant(context.Background(), nil, queryInstantInput{
		Query: `histogram_quantile(0.9, rate(http_requests_total{namespace="shop"}[5m]))`,
		Time:  "2026-07-28T10:15:00Z",
	})
	if err != nil {
		t.Fatalf("handleQueryInstant: %v", err)
	}
	checkGolden(t, "query_instant_annotated", resultText(t, res))
}

// TestGoldenQueryInstantAnnotationsCapped locks the capped shape byte-exact:
// the kept set, the ordering, the totals and the note wording all at once.
//
// The fixture is the nine warnings a real v3.5.0 returns for
// `histogram_quantile(1.5, {job="…"})` — eight per-metric repeats and one
// quantile warning — so the golden is also the readable record of what
// pickAcrossKinds does with them. Read it: four `bucket label` and the one
// `quantile value`, not the five alphabetically first.
func TestGoldenQueryInstantAnnotationsCapped(t *testing.T) {
	warnings := []string{realQuantileWarning}
	for _, m := range []string{
		"kube_pod_container_status_ready",
		"kube_pod_container_status_restarts_total",
		"kube_pod_container_status_waiting_reason",
		"scrape_duration_seconds",
		"scrape_samples_post_metric_relabeling",
		"scrape_samples_scraped",
		"scrape_series_added",
		"up",
	} {
		warnings = append(warnings, fmt.Sprintf(
			`PromQL warning: bucket label "le" is missing or has a malformed value of "" for metric name %q (1:25)`, m))
	}

	fake := goldenFake(t)
	fake.set("/api/v1/query", 200, annotatedBody(t, fake.body("/api/v1/query"), warnings, nil))
	res, _, err := newTestToolServer(fake).handleQueryInstant(context.Background(), nil, queryInstantInput{
		Query: `histogram_quantile(1.5, {job="kube-state-metrics"})`,
	})
	if err != nil {
		t.Fatalf("handleQueryInstant: %v", err)
	}
	checkGolden(t, "query_instant_annotations_capped", resultText(t, res))
}

func TestGoldenSeriesMetadata(t *testing.T) {
	ts := newTestToolServer(goldenFake(t))
	res, _, err := ts.handleSeriesMetadata(context.Background(), nil, seriesMetadataInput{
		Match: `{namespace="shop"}`,
	})
	if err != nil {
		t.Fatalf("handleSeriesMetadata: %v", err)
	}
	checkGolden(t, "series_metadata", resultText(t, res))
}
