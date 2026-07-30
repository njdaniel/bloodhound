//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/bloodhound/internal/promtest"
)

// The upstream caps series_metadata sends to Prometheus — MaxUpstreamSeries on
// /api/v1/series and MaxUpstreamMetadata on /api/v1/metadata — are the one
// part of that tool whose correctness is a claim about the *server*, not about
// bloodhound's shaping. The unit tests in this package can only assert that
// mcp-prom reacts correctly to a fake that was told to truncate. What decides
// whether the reaction is right is behaviour no fake establishes:
//
//   - /api/v1/series announces its truncation in a warning, and the wording of
//     that warning is what seriesCapped pattern-matches on;
//   - /api/v1/metadata truncates with no warning at all, which is why that cap
//     is detected by counting instead;
//   - which metrics survive a metadata truncation is decided by map iteration
//     upstream, so it is arbitrary and varies between two calls to one server.
//
// The S01 fixture in the other integration tests has three metrics, so no cap
// there can ever fire. These tests use a fixture large enough that both do.

// bulkMetricCount is how many distinct metric names the bulk fixture registers.
// Three times the larger cap, and neither part of that is arbitrary.
//
// Three, because metadata truncation keeps an arbitrary subset, so the test
// needs the odds of all MaxMetadataMetrics returned metrics happening to keep
// their metadata to be negligible. At a one-in-three survival rate that is
// (1/3)^25, around 1e-12.
//
// The larger cap, because the fixture has to exceed *both* — one series per
// metric name, so the same count has to overflow MaxUpstreamSeries on
// /api/v1/series and MaxUpstreamMetadata on /api/v1/metadata. The two are
// independently configurable and only happen to be equal today; deriving from
// one of them would silently stop exercising the other half the moment they
// diverge.
const bulkMetricCount = 3 * max(MaxUpstreamMetadata, MaxUpstreamSeries)

// realSeriesTruncationWarning is what Prometheus v3.5.0 actually puts in the
// warnings array of a /api/v1/series response it truncated at the requested
// limit, captured from the wire rather than from documentation.
//
// If the assertion on this string ever fails, the fix is NOT to update the
// constant and move on: seriesCapped in tools.go decides "the server
// truncated" by looking for "truncated" in the warning, and a reworded
// upstream warning that no longer contains it would make series_metadata
// silently report a truncated discovery as a complete one. Treat a failure
// here as a bug report against the matcher.
const realSeriesTruncationWarning = "results truncated due to limit"

// startBulkStack brings up the bulk metrics endpoint plus a Prometheus
// container scraping it, and blocks until every fixture series has landed.
//
// Unlike startStack it waits for series to exist rather than for a counter to
// climb: nothing here is a counter worth watching, and one scrape is all the
// caps need. That is also why the scrape interval is a second — the fixture
// exposition runs to about a megabyte, and there is no reason to re-serve it
// four times a second for the length of the test.
func startBulkStack(t *testing.T) (*promtest.BulkWorkload, *promtest.Server) {
	t.Helper()
	requireDocker(t)

	workload := promtest.StartBulkWorkload(bulkMetricCount)
	t.Cleanup(workload.Close)

	srv, err := promtest.Start(t.Context(), promtest.Config{
		Targets:        []string{workload.HostPort()},
		JobName:        promtest.BulkJobName,
		ScrapeInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("starting prometheus: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(); err != nil {
			t.Errorf("stopping prometheus: %v", err)
		}
	})

	ready := fmt.Sprintf("count({job=%q}) >= %d", promtest.BulkJobName, workload.Count())
	if err := srv.WaitForSamples(t.Context(), ready, 1); err != nil {
		logs, _ := srv.Logs(t.Context())
		t.Fatalf("bulk fixture never fully landed: %v\ncontainer logs:\n%s", err, logs)
	}
	return workload, srv
}

// promAPI performs one raw GET against the Prometheus HTTP API, decodes the
// envelope's data into out, and returns the response's warnings. It exists so
// a test can assert on what the server sent before mcp-prom reshaped it —
// these tests are as much about pinning Prometheus's behaviour as bloodhound's
// reaction to it.
func promAPI(t *testing.T, srv *promtest.Server, path string, params url.Values, out any) []string {
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
	var env struct {
		Status   string          `json:"status"`
		Data     json.RawMessage `json:"data"`
		Warnings []string        `json:"warnings"`
		Error    string          `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decoding prometheus %s response (HTTP %d): %v", path, resp.StatusCode, err)
	}
	if env.Status != "success" {
		t.Fatalf("prometheus %s returned status %q: %s", path, env.Status, env.Error)
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			t.Fatalf("decoding prometheus %s data: %v", path, err)
		}
	}
	return env.Warnings
}

// wireMetadataResult mirrors a series_metadata payload for decoding, for the
// same reason wireRangeResult exists: the server's own types are write-only.
type wireMetadataResult struct {
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

// TestSeriesMetadataUpstreamCapsAgainstRealPrometheus makes both upstream caps
// series_metadata sends to Prometheus fire against a real server. One
// container serves both halves: the wire half pins the server behaviour the
// cap detection is built on, and the tool half checks mcp-prom reports it.
func TestSeriesMetadataUpstreamCapsAgainstRealPrometheus(t *testing.T) {
	workload, srv := startBulkStack(t)
	selector := fmt.Sprintf("{job=%q}", promtest.BulkJobName)

	t.Run("wire", func(t *testing.T) { capWireBehaviour(t, workload, srv, selector) })
	t.Run("tool", func(t *testing.T) { capToolReport(t, srv, selector) })
}

// capWireBehaviour asserts, at the wire level, that a truncated /api/v1/series
// says so in a warning whose wording seriesCapped can recognise, and that a
// truncated /api/v1/metadata says nothing at all.
func capWireBehaviour(t *testing.T, workload *promtest.BulkWorkload, srv *promtest.Server, selector string) {
	// Deliberately not t.Helper(): this function *is* the subtest, not an
	// assertion helper, and marking it would collapse every failure below onto
	// the t.Run line instead of the assertion that fired.

	// The tool's own window, so the call under test is the call being pinned.
	end := time.Now()
	start := end.Add(-MetadataLookback)
	window := url.Values{
		"match[]": {selector},
		"start":   {formatTime(start)},
		"end":     {formatTime(end)},
	}

	// Neither cap can fire against a fixture that does not overflow it, and a
	// fixture that is too small fails later as a confusing truncation error
	// rather than as itself. Check both up front, before any assertion that
	// would misattribute the cause. bulkMetricCount is derived from the larger
	// cap so this should be unreachable; it is here because the caps are
	// independently configurable and a future edit is exactly what would break
	// the derivation.
	if workload.Count() <= MaxUpstreamSeries {
		t.Fatalf("fixture exports %d series, want more than MaxUpstreamSeries (%d) or the series cap cannot fire", workload.Count(), MaxUpstreamSeries)
	}
	if workload.Count() <= MaxUpstreamMetadata {
		t.Fatalf("fixture exports %d metrics, want more than MaxUpstreamMetadata (%d) or the metadata cap cannot fire", workload.Count(), MaxUpstreamMetadata)
	}

	// Control: with no limit the same selector returns every fixture series and
	// the server is silent. Without this, a warning below could be ambient
	// rather than caused by the limit.
	var uncapped []map[string]string
	if warnings := promAPI(t, srv, "/api/v1/series", window, &uncapped); len(warnings) != 0 {
		t.Fatalf("unlimited /api/v1/series warned %q; the fixture is meant to be quiet until a limit is applied", warnings)
	}
	if len(uncapped) < workload.Count() {
		t.Fatalf("unlimited /api/v1/series returned %d series, want at least the %d the fixture exports", len(uncapped), workload.Count())
	}

	// The real thing: limit=MaxUpstreamSeries, the value series_metadata sends.
	capped := url.Values{}
	for k, v := range window {
		capped[k] = v
	}
	capped.Set("limit", strconv.Itoa(MaxUpstreamSeries))
	var cappedSets []map[string]string
	warnings := promAPI(t, srv, "/api/v1/series", capped, &cappedSets)
	if len(cappedSets) != MaxUpstreamSeries {
		t.Errorf("/api/v1/series with limit=%d returned %d series, want exactly the limit", MaxUpstreamSeries, len(cappedSets))
	}
	if len(warnings) != 1 {
		t.Fatalf("/api/v1/series truncation produced %d warnings %q, want exactly one", len(warnings), warnings)
	}
	// Exact text first: this is the assertion that tells us the wording moved.
	if warnings[0] != realSeriesTruncationWarning {
		t.Errorf("real /api/v1/series truncation warning is %q, want %q.\n"+
			"This is a finding, not a test fixup: seriesCapped in tools.go classifies warnings by "+
			"looking for %q, so re-check that matcher against the new wording before touching this test.",
			warnings[0], realSeriesTruncationWarning, "truncated")
	}
	// Then the property the matcher actually depends on, spelled out the same
	// way tools.go spells it, so the coupling is visible in one place.
	if !strings.Contains(strings.ToLower(warnings[0]), "truncated") {
		t.Errorf("seriesCapped would not fire on the real warning %q: it matches on %q", warnings[0], "truncated")
	}

	// /api/v1/metadata: the whole point of detecting this cap by count is that
	// there is nothing else to detect it by.
	var fullMeta map[string]json.RawMessage
	promAPI(t, srv, "/api/v1/metadata", nil, &fullMeta)
	var cappedMeta map[string]json.RawMessage
	metaWarnings := promAPI(t, srv, "/api/v1/metadata",
		url.Values{"limit": {strconv.Itoa(MaxUpstreamMetadata)}}, &cappedMeta)
	if len(cappedMeta) != MaxUpstreamMetadata {
		t.Errorf("/api/v1/metadata with limit=%d returned %d metrics, want exactly the limit", MaxUpstreamMetadata, len(cappedMeta))
	}
	if len(metaWarnings) != 0 {
		t.Errorf("/api/v1/metadata truncation warned %q. It has always truncated silently, which is why "+
			"metadataCapped counts instead — if that changed, counting can be replaced by reading the warning.",
			metaWarnings)
	}

	// And the arbitrariness: two identical calls to one server need not agree
	// on which metrics survive. Asserting they *differ* would be flaky, so this
	// only records the overlap — the assertions in the tool-level test are
	// written to hold whichever way it lands.
	var secondMeta map[string]json.RawMessage
	promAPI(t, srv, "/api/v1/metadata",
		url.Values{"limit": {strconv.Itoa(MaxUpstreamMetadata)}}, &secondMeta)
	common := 0
	for name := range cappedMeta {
		if _, ok := secondMeta[name]; ok {
			common++
		}
	}
	t.Logf("two identical /api/v1/metadata?limit=%d calls shared %d of %d metrics; survivors are upstream map order",
		MaxUpstreamMetadata, common, len(cappedMeta))
}

// capToolReport drives the real mcp-prom binary against a Prometheus holding
// more metrics than either upstream cap allows, and checks that both caps are
// reported to the model.
func capToolReport(t *testing.T, srv *promtest.Server, selector string) {
	// Not t.Helper(), for the same reason as capWireBehaviour.
	session := connect(t, srv.URL)

	var got wireMetadataResult
	callTool(t, session, "series_metadata", map[string]any{"match": selector}, &got)

	// The series cap fired: the tool only ever saw MaxUpstreamSeries series,
	// and the fixture exports one series per metric name, so that is also how
	// many metrics it could possibly have counted — far fewer than exist.
	if got.Truncation.MetricsTotal != MaxUpstreamSeries {
		t.Errorf("metrics_total = %d, want %d: the fixture exports one series per metric, so a lookup "+
			"truncated at %d series yields exactly that many metric names",
			got.Truncation.MetricsTotal, MaxUpstreamSeries, MaxUpstreamSeries)
	}
	if got.Truncation.MetricsReturned != MaxMetadataMetrics {
		t.Errorf("metrics_returned = %d, want the %d-metric output cap", got.Truncation.MetricsReturned, MaxMetadataMetrics)
	}
	if len(got.Metrics) != got.Truncation.MetricsReturned {
		t.Errorf("payload carries %d metrics but reports %d returned", len(got.Metrics), got.Truncation.MetricsReturned)
	}

	note := got.Truncation.Note
	for _, want := range []string{
		fmt.Sprintf("%d-series limit", MaxUpstreamSeries),   // seriesCapped, from the real warning
		fmt.Sprintf("%d-metric limit", MaxUpstreamMetadata), // metadataCapped, from the silent truncation
		"Narrow the match selector.",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("truncation note %q does not mention %q", note, want)
		}
	}
	// The truncation warning must be consumed by the classifier, not leaked
	// through the verbatim path: if the real wording ever stopped matching, the
	// note would carry "Prometheus warned: results truncated due to limit"
	// instead of the series-limit sentence, and the model would be told a
	// truncation was some other backend problem.
	if strings.Contains(note, "Prometheus warned:") {
		t.Errorf("truncation note %q passed a warning through verbatim; the only warning here is the "+
			"truncation one and seriesCapped should have classified it", note)
	}

	// Which metrics kept their metadata is upstream map order. Nothing below
	// names a metric or depends on position: the fixture registers HELP and
	// TYPE for every one of its metrics, so any metric that arrives without
	// them lost them to the metadata cap and no other cause is possible.
	missing := 0
	names := make([]string, 0, len(got.Metrics))
	for _, m := range got.Metrics {
		names = append(names, m.Name)
		if !strings.HasPrefix(m.Name, promtest.BulkMetricPrefix) {
			t.Errorf("metric %q is not a fixture metric; the selector should only match the bulk job", m.Name)
			continue
		}
		if shards := m.Labels[promtest.BulkShardLabel]; len(shards) != 1 || shards[0] != promtest.BulkShardValue {
			t.Errorf("metric %q has %s = %v, want [%q] from the exposition", m.Name, promtest.BulkShardLabel, shards, promtest.BulkShardValue)
		}
		if m.Type == "" || m.Help == "" {
			missing++
			continue
		}
		// Survivors must carry the fixture's real metadata, so a "present"
		// field cannot be some placeholder the tool invented.
		if m.Type != promtest.BulkMetricType {
			t.Errorf("metric %q type = %q, want %q", m.Name, m.Type, promtest.BulkMetricType)
		}
		if !strings.HasPrefix(m.Help, promtest.BulkHelpPrefix) {
			t.Errorf("metric %q help = %q, want the exposition HELP text", m.Name, m.Help)
		}
	}
	if missing == 0 {
		t.Errorf("every one of the %d returned metrics kept its metadata, so metaIncomplete was never set. "+
			"With %d metrics registered against a %d-metric upstream limit this should be about a "+
			"one-in-1e12 event — suspect the limit stopped being applied rather than bad luck.",
			len(got.Metrics), bulkMetricCount, MaxUpstreamMetadata)
	}
	t.Logf("%d of %d returned metrics lost their type/help to the upstream metadata limit", missing, len(got.Metrics))

	// The tool's own ordering contract still holds under both caps.
	if !sort.StringsAreSorted(names) {
		t.Errorf("metrics are not alphabetical: %v", names)
	}
}
