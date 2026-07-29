//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/njdaniel/bloodhound/internal/promtest"
)

// These tests drive the real mcp-prom binary against a real Prometheus
// container scraping a real (test-owned) /metrics endpoint. The httptest-fake
// unit tests in this package pin the shaping logic against canned JSON; these
// exist to catch wire-format drift between mcp-prom's stdlib Prometheus
// client and an actual server — response envelopes, metadata shapes, the
// labels Prometheus itself attaches. Nothing here calls a paid API.
//
// Run with `make test-integration`. Without docker they skip, so `make check`
// on a laptop without docker still passes.

// minSamples is how many scrapes must land before assertions run. At the
// 250ms fixture interval that is ten seconds of data, which is enough for a
// two-minute range query (clamped to a 1s step) to return a handful of points.
const minSamples = 40

// requireDocker skips the test when no docker daemon is reachable.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := promtest.CheckDocker(t.Context()); err != nil {
		t.Skipf("skipping real-Prometheus integration test: %v\n"+
			"Install docker and re-run `make test-integration`; `make check` does not need it.", err)
	}
}

// startStack brings up the fixture metrics endpoint plus a Prometheus
// container scraping it, and blocks until real samples have landed.
func startStack(t *testing.T) (*promtest.Workload, *promtest.Server) {
	t.Helper()
	requireDocker(t)

	workload := promtest.StartWorkload()
	t.Cleanup(workload.Close)

	srv, err := promtest.Start(t.Context(), promtest.Config{Targets: []string{workload.HostPort()}})
	if err != nil {
		t.Fatalf("starting prometheus: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(); err != nil {
			t.Errorf("stopping prometheus: %v", err)
		}
	})

	// Poll until Prometheus has actually stored enough samples; asserting
	// before the first scrape lands would test an empty result set.
	ready := fmt.Sprintf("count_over_time(%s{pod=%q}[5m]) >= %d", promtest.RestartsMetric, promtest.BrokenPod, minSamples)
	if err := srv.WaitForSamples(t.Context(), ready, 1); err != nil {
		logs, _ := srv.Logs(t.Context())
		t.Fatalf("no samples landed: %v\ncontainer logs:\n%s", err, logs)
	}
	t.Logf("prometheus %s scraped %s %d times", srv.URL, workload.URL(), workload.Scrapes())
	return workload, srv
}

// connect spawns the built mcp-prom binary pointed at promURL and returns a
// connected MCP client session.
func connect(t *testing.T, promURL string) *mcp.ClientSession {
	t.Helper()
	bin := buildServerBinary(t)

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "PROM_URL="+promURL, "PROM_TIMEOUT=10s")
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-prom-integration", Version: "0.0.1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting to mcp-prom over stdio: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// callTool invokes one tool and returns the decoded JSON payload, failing the
// test on a protocol failure or a tool error.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("calling %s returned a tool error: %s", name, text)
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		t.Fatalf("decoding %s result %s: %v", name, text, err)
	}
}

// callToolError invokes one tool expecting an actionable MCP tool error, and
// returns its message.
func callToolError(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s became a protocol failure, want a tool error: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("calling %s succeeded, want a tool error: %s", name, resultText(t, res))
	}
	return resultText(t, res)
}

// The types below mirror the query_range payload for decoding. The server's
// own types marshal through custom marshalers and are write-only; decoding
// into a plain mirror asserts on the wire form the model actually sees.

// wireStats mirrors a series' stats block.
type wireStats struct {
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Avg  float64 `json:"avg"`
	Last float64 `json:"last"`
}

// wireSeries mirrors one query_range series. Points stay raw so the test can
// assert the [unix_seconds, "value"] wire form rather than a decoded shape.
type wireSeries struct {
	Labels map[string]string    `json:"labels"`
	Stats  wireStats            `json:"stats"`
	Points [][2]json.RawMessage `json:"points"`
}

// pod returns the series' pod label, for readable assertions.
func (s wireSeries) pod() string { return s.Labels["pod"] }

// wireRangeTruncation mirrors the query_range truncation block.
type wireRangeTruncation struct {
	SeriesTotal    int    `json:"series_total"`
	SeriesReturned int    `json:"series_returned"`
	PointsThinned  bool   `json:"points_thinned"`
	Note           string `json:"note"`
}

// wireRangeResult mirrors a whole query_range payload.
type wireRangeResult struct {
	ResolvedStepSeconds int64               `json:"resolved_step_seconds"`
	Series              []wireSeries        `json:"series"`
	Truncation          wireRangeTruncation `json:"truncation"`
}

func TestQueryRangeAgainstRealPrometheus(t *testing.T) {
	_, srv := startStack(t)
	session := connect(t, srv.URL)

	end := time.Now().UTC()
	start := end.Add(-2 * time.Minute)
	var got wireRangeResult
	callTool(t, session, "query_range", map[string]any{
		"query": fmt.Sprintf("%s{namespace=%q}", promtest.RestartsMetric, promtest.Namespace),
		"start": start.Format(time.RFC3339),
		"end":   end.Format(time.RFC3339),
	}, &got)

	// Step clamp: a 120s window with the step omitted resolves to 1s, the
	// smallest step that keeps a series under 120 points.
	if got.ResolvedStepSeconds != 1 {
		t.Errorf("resolved_step_seconds = %d, want 1 for a 120s window", got.ResolvedStepSeconds)
	}
	// Both fixture pods are exported, so the real server returns two series
	// and nothing is dropped.
	if got.Truncation.SeriesTotal != 2 || got.Truncation.SeriesReturned != 2 {
		t.Errorf("truncation = %+v, want 2 series total and returned", got.Truncation)
	}
	if got.Truncation.PointsThinned {
		t.Errorf("points_thinned = true; a two-minute window must fit the %d byte cap", MaxResponseBytes)
	}
	if got.Truncation.Note != "" {
		t.Errorf("truncation note = %q, want empty when nothing was capped", got.Truncation.Note)
	}

	var sawBroken bool
	for _, s := range got.Series {
		// Labels only a real Prometheus adds: the scrape job and the target
		// instance are attached by the server, not by the exposition.
		if s.Labels["job"] != promtest.JobName {
			t.Errorf("series %v has job=%q, want %q — Prometheus target labels are missing", s.Labels, s.Labels["job"], promtest.JobName)
		}
		if s.Labels["instance"] == "" {
			t.Errorf("series %v has no instance label", s.Labels)
		}
		if len(s.Points) < 5 {
			t.Errorf("series %v returned %d points, want at least 5 after %d scrapes", s.Labels, len(s.Points), minSamples)
		}
		// Wire form of a point is [unix_seconds, "value"]: a bare number and a
		// quoted string, in that order.
		for i, p := range s.Points {
			ts, err := strconv.ParseInt(string(p[0]), 10, 64)
			if err != nil {
				t.Fatalf("point %d timestamp %s is not a bare integer: %v", i, p[0], err)
			}
			if ts < start.Unix() || ts > end.Unix() {
				t.Errorf("point %d timestamp %d falls outside the requested window [%d,%d]", i, ts, start.Unix(), end.Unix())
			}
			var v string
			if err := json.Unmarshal(p[1], &v); err != nil {
				t.Fatalf("point %d value %s is not a JSON string: %v", i, p[1], err)
			}
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				t.Errorf("point %d value %q does not parse as a float: %v", i, v, err)
			}
		}
		if s.Stats.Min > s.Stats.Max {
			t.Errorf("series %v stats min %v > max %v", s.Labels, s.Stats.Min, s.Stats.Max)
		}
		if s.pod() == promtest.BrokenPod {
			sawBroken = true
			// The fixture counter climbs on every scrape, so the broken pod's
			// series must be genuinely non-flat — this is what proves the
			// samples came from live scraping and not from a replayed fixture.
			if s.Stats.Max <= s.Stats.Min {
				t.Errorf("broken pod stats %+v are flat; the restart counter should be climbing", s.Stats)
			}
			if s.Stats.Last != s.Stats.Max {
				t.Errorf("broken pod last %v != max %v for a monotonic counter", s.Stats.Last, s.Stats.Max)
			}
		}
	}
	if !sawBroken {
		t.Errorf("no series for pod %q in %d results", promtest.BrokenPod, len(got.Series))
	}

	// Ranking is by (max−min) descending: the climbing counter must outrank
	// the flat one.
	if len(got.Series) == 2 && got.Series[0].pod() != promtest.BrokenPod {
		t.Errorf("series order = %v then %v, want the volatile pod %q first",
			got.Series[0].pod(), got.Series[1].pod(), promtest.BrokenPod)
	}
}

func TestQueryInstantAgainstRealPrometheus(t *testing.T) {
	_, srv := startStack(t)
	session := connect(t, srv.URL)

	var got struct {
		ResultType string `json:"result_type"`
		Samples    []struct {
			Labels    map[string]string `json:"labels"`
			Value     string            `json:"value"`
			Timestamp int64             `json:"timestamp"`
		} `json:"samples"`
		Truncation struct {
			SamplesTotal    int `json:"samples_total"`
			SamplesReturned int `json:"samples_returned"`
		} `json:"truncation"`
	}
	// `up` is synthesized by Prometheus itself; a value of 1 is proof the
	// scrape of the test-owned endpoint is actually succeeding.
	callTool(t, session, "query_instant", map[string]any{
		"query": fmt.Sprintf("up{job=%q}", promtest.JobName),
	}, &got)

	if got.ResultType != "vector" {
		t.Errorf("result_type = %q, want vector", got.ResultType)
	}
	if got.Truncation.SamplesTotal != 1 || got.Truncation.SamplesReturned != 1 {
		t.Fatalf("truncation = %+v, want exactly one up series", got.Truncation)
	}
	sample := got.Samples[0]
	if sample.Value != "1" {
		t.Errorf("up = %q, want \"1\" — Prometheus is not scraping the fixture target", sample.Value)
	}
	if sample.Labels["instance"] == "" {
		t.Errorf("up sample %v has no instance label", sample.Labels)
	}
	// The timestamp is the server's evaluation time, not ours: it must be
	// recent, and it must be unix seconds rather than the milliseconds the
	// HTTP API speaks in some places. A second of slack in the future absorbs
	// clock skew between the container and this process plus the truncation
	// of sub-second precision — the assertion is about the magnitude, which
	// is off by a factor of a thousand if the unit ever changes.
	if age := time.Since(time.Unix(sample.Timestamp, 0)); age < -5*time.Second || age > 5*time.Minute {
		t.Errorf("sample timestamp %d is %v old; expected recent unix seconds", sample.Timestamp, age)
	}

	// A gauge with a reason label: this is the S01 signal a metrics-hound
	// would actually pull.
	var crash struct {
		Samples []struct {
			Labels map[string]string `json:"labels"`
			Value  string            `json:"value"`
		} `json:"samples"`
	}
	callTool(t, session, "query_instant", map[string]any{
		"query": fmt.Sprintf("%s{reason=\"CrashLoopBackOff\"} == 1", promtest.WaitingReasonMetric),
	}, &crash)
	if len(crash.Samples) != 1 {
		t.Fatalf("CrashLoopBackOff samples = %d, want exactly the broken pod", len(crash.Samples))
	}
	if pod := crash.Samples[0].Labels["pod"]; pod != promtest.BrokenPod {
		t.Errorf("CrashLoopBackOff pod = %q, want %q", pod, promtest.BrokenPod)
	}
}

func TestSeriesMetadataAgainstRealPrometheus(t *testing.T) {
	_, srv := startStack(t)
	session := connect(t, srv.URL)

	var got struct {
		Metrics []struct {
			Name   string              `json:"name"`
			Type   string              `json:"type"`
			Help   string              `json:"help"`
			Labels map[string][]string `json:"labels"`
		} `json:"metrics"`
		Truncation struct {
			MetricsTotal    int `json:"metrics_total"`
			MetricsReturned int `json:"metrics_returned"`
		} `json:"truncation"`
	}
	callTool(t, session, "series_metadata", map[string]any{
		"match": fmt.Sprintf("{namespace=%q}", promtest.Namespace),
	}, &got)

	byName := map[string]int{}
	for i, m := range got.Metrics {
		byName[m.Name] = i
	}
	for _, want := range []string{promtest.RestartsMetric, promtest.WaitingReasonMetric, promtest.ReadyMetric} {
		if _, ok := byName[want]; !ok {
			t.Errorf("series_metadata did not discover %q; got %v", want, byName)
		}
	}
	if got.Truncation.MetricsTotal != got.Truncation.MetricsReturned {
		t.Errorf("truncation = %+v, want nothing dropped for three fixture metrics", got.Truncation)
	}
	// Metrics must come back alphabetically — this is a discovery tool, so
	// determinism beats relevance (spec 002 §2.2).
	for i := 1; i < len(got.Metrics); i++ {
		if got.Metrics[i-1].Name > got.Metrics[i].Name {
			t.Errorf("metrics are not alphabetical: %q before %q", got.Metrics[i-1].Name, got.Metrics[i].Name)
		}
	}

	// Type and help come from /api/v1/metadata, whose response shape is the
	// most drift-prone corner of the Prometheus API; a fake cannot catch a
	// change here.
	restarts := got.Metrics[byName[promtest.RestartsMetric]]
	if restarts.Type != "counter" {
		t.Errorf("%s type = %q, want counter from /api/v1/metadata", promtest.RestartsMetric, restarts.Type)
	}
	if !strings.Contains(restarts.Help, "restarts") {
		t.Errorf("%s help = %q, want the exposition HELP text", promtest.RestartsMetric, restarts.Help)
	}
	pods := restarts.Labels["pod"]
	if len(pods) != 2 || pods[0] != promtest.HealthyPod || pods[1] != promtest.BrokenPod {
		t.Errorf("%s pod label values = %v, want both fixture pods sorted", promtest.RestartsMetric, pods)
	}

	reason := got.Metrics[byName[promtest.WaitingReasonMetric]]
	if reason.Type != "gauge" {
		t.Errorf("%s type = %q, want gauge", promtest.WaitingReasonMetric, reason.Type)
	}
	if reasons := reason.Labels["reason"]; len(reasons) != 2 {
		t.Errorf("%s reason label values = %v, want CrashLoopBackOff and ImagePullBackOff", promtest.WaitingReasonMetric, reasons)
	}
}

func TestGuardrailsAgainstRealPrometheus(t *testing.T) {
	_, srv := startStack(t)
	session := connect(t, srv.URL)

	end := time.Now().UTC()

	// The 24h range limit is an error, not a clamp, even when the server
	// would happily answer.
	msg := callToolError(t, session, "query_range", map[string]any{
		"query": promtest.RestartsMetric,
		"start": end.Add(-36 * time.Hour).Format(time.RFC3339),
		"end":   end.Format(time.RFC3339),
	})
	if !strings.Contains(msg, "36h") || !strings.Contains(msg, "24h") {
		t.Errorf("range-limit error = %q, want it to name the 36h window and the 24h limit", msg)
	}

	// Bad PromQL comes back as a tool error carrying the real server's
	// parse message, so the model can correct itself.
	msg = callToolError(t, session, "query_instant", map[string]any{"query": "sum(("})
	if !strings.Contains(msg, "prometheus rejected the request") {
		t.Errorf("bad-PromQL error = %q, want the upstream rejection wrapped", msg)
	}
}
