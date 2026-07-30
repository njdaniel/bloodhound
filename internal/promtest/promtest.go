// Package promtest runs a real Prometheus in a container against a
// test-owned scrape target. It backs the `-tags integration` tests
// (`make test-integration`, spec 002 §5): the httptest-fake unit tests pin
// bloodhound's own shaping logic, and these catch wire-format drift between
// mcp-prom and an actual Prometheus server.
//
// The container is driven with os/exec and the docker CLI rather than a
// docker SDK — that keeps the stdlib-first rule (CLAUDE.md) intact for a
// dependency that would only ever be used by tests.
//
// Prometheus runs on the host network so it can scrape the httptest server
// the test process is serving on 127.0.0.1, which is also why the server
// binds a port the caller picked rather than the default 9090.
package promtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultImage is the Prometheus container image used when Config.Image is
// empty. Pinned so a wire-format change arrives as a deliberate bump in the
// diff rather than as a surprise on a Tuesday.
const DefaultImage = "prom/prometheus:v3.5.0"

// ImageEnv names the environment variable that overrides DefaultImage, for
// running the suite against a different Prometheus version.
const ImageEnv = "BLOODHOUND_PROM_IMAGE"

// DefaultScrapeInterval is how often Prometheus scrapes the test target.
// Deliberately sub-second so a test can wait for real samples in a few
// hundred milliseconds instead of a minute (spec 002 §5).
const DefaultScrapeInterval = 250 * time.Millisecond

// ErrNoDocker reports that no usable docker daemon was found. Callers skip
// rather than fail on it: `make check` must pass on a laptop without docker.
type ErrNoDocker struct {
	// Reason is the underlying failure, e.g. the docker CLI not being on PATH.
	Reason error
}

// Error implements error.
func (e *ErrNoDocker) Error() string {
	return fmt.Sprintf("docker is not available: %v", e.Reason)
}

// Unwrap returns the underlying failure.
func (e *ErrNoDocker) Unwrap() error { return e.Reason }

// CheckDocker reports whether a docker daemon is reachable. It returns an
// *ErrNoDocker when it is not, so tests can skip with a clear message.
func CheckDocker(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	path, err := exec.LookPath("docker")
	if err != nil {
		return &ErrNoDocker{Reason: fmt.Errorf("looking for the docker CLI: %w", err)}
	}
	cmd := exec.CommandContext(ctx, path, "info", "--format", "{{.ServerVersion}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &ErrNoDocker{Reason: fmt.Errorf("running docker info: %w: %s", err, strings.TrimSpace(string(out)))}
	}
	return nil
}

// Config configures the Prometheus container.
type Config struct {
	// Image is the container image; DefaultImage (or the ImageEnv override)
	// when empty.
	Image string
	// Targets are host:port addresses Prometheus scrapes at /metrics.
	Targets []string
	// JobName labels the scrape job; JobName when empty.
	JobName string
	// ScrapeInterval is the scrape period; DefaultScrapeInterval when zero.
	ScrapeInterval time.Duration
}

// JobName is the default Prometheus job label for the test scrape target.
// It mirrors kube-state-metrics because the fixture metrics do (see Workload).
const JobName = "kube-state-metrics"

// Server is a running Prometheus container.
type Server struct {
	// URL is the Prometheus base URL, e.g. "http://127.0.0.1:34567".
	URL string

	container string
	configDir string
}

// Start launches Prometheus in a container on the host network, scraping
// cfg.Targets, and waits for it to report ready. The caller must Stop it.
func Start(ctx context.Context, cfg Config) (_ *Server, err error) {
	if len(cfg.Targets) == 0 {
		return nil, fmt.Errorf("starting prometheus: no scrape targets given")
	}
	image := cfg.Image
	if image == "" {
		image = DefaultImage
		if override := os.Getenv(ImageEnv); override != "" {
			image = override
		}
	}
	job := cfg.JobName
	if job == "" {
		job = JobName
	}
	interval := cfg.ScrapeInterval
	if interval <= 0 {
		interval = DefaultScrapeInterval
	}

	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("starting prometheus: %w", err)
	}

	// The image runs as nobody, so both the config dir and the file inside it
	// must be world-readable; os.MkdirTemp creates 0700.
	dir, err := os.MkdirTemp("", "bloodhound-promtest-*")
	if err != nil {
		return nil, fmt.Errorf("starting prometheus: creating config dir: %w", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, fmt.Errorf("starting prometheus: relaxing config dir permissions: %w", err)
	}
	cfgPath := filepath.Join(dir, "prometheus.yml")
	if err := os.WriteFile(cfgPath, []byte(renderConfig(job, interval, cfg.Targets)), 0o644); err != nil {
		return nil, fmt.Errorf("starting prometheus: writing config: %w", err)
	}

	out, err := exec.CommandContext(ctx, "docker", "run", "--detach", "--rm",
		"--network", "host",
		"--volume", cfgPath+":/etc/prometheus/prometheus.yml:ro",
		image,
		"--config.file=/etc/prometheus/prometheus.yml",
		"--web.listen-address=127.0.0.1:"+port,
		"--storage.tsdb.retention.time=1h",
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("starting prometheus container: %w: %s", err, strings.TrimSpace(string(out)))
	}
	srv := &Server{
		URL:       "http://127.0.0.1:" + port,
		container: strings.TrimSpace(string(out)),
		configDir: dir,
	}
	if err := srv.waitReady(ctx); err != nil {
		logs, _ := srv.Logs(ctx)
		// Stop deliberately does not take the caller's context: ctx is the one
		// that just expired or was cancelled, and the container has to be
		// removed either way or it outlives the test run.
		stopErr := srv.Stop() //nolint:contextcheck // cleanup must not inherit the failed context
		if stopErr != nil {
			return nil, fmt.Errorf("%w (and stopping the container failed: %w); container logs:\n%s", err, stopErr, logs)
		}
		return nil, fmt.Errorf("%w; container logs:\n%s", err, logs)
	}
	return srv, nil
}

// renderConfig builds the prometheus.yml the container runs with. The scrape
// timeout must not exceed the interval or Prometheus refuses to start, so it
// is derived from it rather than left at the 10s default.
func renderConfig(job string, interval time.Duration, targets []string) string {
	timeout := interval * 4 / 5
	if timeout <= 0 {
		timeout = interval
	}
	var b strings.Builder
	fmt.Fprintf(&b, "global:\n  scrape_interval: %s\n  scrape_timeout: %s\n  evaluation_interval: %s\n",
		duration(interval), duration(timeout), duration(interval))
	fmt.Fprintf(&b, "scrape_configs:\n  - job_name: %q\n    static_configs:\n      - targets:\n", job)
	for _, t := range targets {
		fmt.Fprintf(&b, "          - %q\n", t)
	}
	return b.String()
}

// duration renders a Go duration in the millisecond form Prometheus parses.
func duration(d time.Duration) string {
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// Stop removes the container and its config directory. It is safe to call on
// an already-stopped server.
func (s *Server) Stop() error {
	if s == nil {
		return nil
	}
	var errs []error
	if s.container != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", "rm", "--force", s.container).CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("removing prometheus container: %w: %s", err, strings.TrimSpace(string(out))))
		}
		s.container = ""
	}
	if s.configDir != "" {
		if err := os.RemoveAll(s.configDir); err != nil {
			errs = append(errs, fmt.Errorf("removing prometheus config dir: %w", err))
		}
		s.configDir = ""
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// Logs returns the container's combined output, for diagnosing a failed start.
func (s *Server) Logs(ctx context.Context) (string, error) {
	if s.container == "" {
		return "", nil
	}
	out, err := exec.CommandContext(ctx, "docker", "logs", s.container).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("reading prometheus container logs: %w", err)
	}
	return string(out), nil
}

// waitReady polls /-/ready until Prometheus answers or ctx expires.
func (s *Server) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	hc := &http.Client{Timeout: 2 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/-/ready", nil)
		if err != nil {
			return fmt.Errorf("building readiness request: %w", err)
		}
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for prometheus to become ready at %s: %w", s.URL, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// WaitForSamples polls the instant query API until expr returns at least
// wantSamples samples, so a test asserts on data that has actually landed
// rather than on an empty result from a Prometheus that has not scraped yet.
func (s *Server) WaitForSamples(ctx context.Context, expr string, wantSamples int) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	hc := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for {
		n, err := s.instantSampleCount(ctx, hc, expr)
		switch {
		case err != nil:
			lastErr = err
		case n >= wantSamples:
			return nil
		default:
			lastErr = fmt.Errorf("%q returned %d samples, want %d", expr, n, wantSamples)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for samples of %q: %w (last: %w)", expr, ctx.Err(), lastErr)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// instantSampleCount runs one instant query and reports how many samples came
// back.
func (s *Server) instantSampleCount(ctx context.Context, hc *http.Client, expr string) (int, error) {
	u := s.URL + "/api/v1/query?" + url.Values{"query": {expr}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("building query request: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("querying prometheus: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		Status string `json:"status"`
		Data   struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decoding query response: %w", err)
	}
	if body.Status != "success" {
		return 0, fmt.Errorf("prometheus returned status %q", body.Status)
	}
	return len(body.Data.Result), nil
}

// freePort reserves and releases a loopback port, returning it as a string.
// The gap between release and the container binding it is a race in theory;
// in practice the ephemeral range does not wrap around that fast, and the
// alternative is a docker SDK dependency.
func freePort() (string, error) {
	// A context would buy nothing here: the listen is on loopback with no
	// resolution step, so it either returns immediately or not at all.
	l, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // loopback listen; nothing to cancel
	if err != nil {
		return "", fmt.Errorf("reserving a port: %w", err)
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		return "", fmt.Errorf("splitting reserved address: %w", err)
	}
	return port, nil
}

// Fixture label values. The metrics Workload exports mirror
// kube-state-metrics for the S01 "bad image tag → CrashLoopBackOff" eval
// scenario (evals/scenarios/S01-bad-image-tag), so the integration tests and
// the demo walkthrough reason about the same shape a real cluster produces.
const (
	// Namespace is the namespace label on every fixture series.
	Namespace = "shop"
	// BrokenPod is the pod stuck in CrashLoopBackOff.
	BrokenPod = "checkout-7d9f"
	// HealthyPod is a running pod, present so queries return more than one
	// series and label-value discovery has something to discriminate.
	HealthyPod = "checkout-6c4a"
	// Container is the container name on both pods.
	Container = "checkout"

	// RestartsMetric is a counter that climbs on every scrape of the broken
	// pod, giving query_range a real, non-flat series to shape.
	RestartsMetric = "kube_pod_container_status_restarts_total"
	// WaitingReasonMetric is the gauge that carries the CrashLoopBackOff
	// reason label.
	WaitingReasonMetric = "kube_pod_container_status_waiting_reason"
	// ReadyMetric reports container readiness.
	ReadyMetric = "kube_pod_container_status_ready"
)

// Workload is an httptest server exposing Prometheus metrics shaped like the
// S01 scenario: one pod in CrashLoopBackOff with a restart count that climbs
// on every scrape, and one healthy pod. The test process owns it, so no
// cluster is required.
type Workload struct {
	srv      *httptest.Server
	restarts atomic.Int64
}

// StartWorkload starts the fixture metrics endpoint on 127.0.0.1.
func StartWorkload() *Workload {
	w := &Workload{}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		// Each scrape advances the restart counter, so a range query over the
		// live window sees a genuinely rising counter rather than a constant.
		restarts := w.restarts.Add(1)
		fmt.Fprint(rw, w.exposition(restarts))
	})
	w.srv = httptest.NewServer(mux)
	return w
}

// exposition renders the fixture metrics in the Prometheus text format. The
// HELP and TYPE lines matter: series_metadata reads them back out of the
// server's /api/v1/metadata, so they are part of what the test asserts.
func (w *Workload) exposition(restarts int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP %s The number of container restarts per container.\n", RestartsMetric)
	fmt.Fprintf(&b, "# TYPE %s counter\n", RestartsMetric)
	fmt.Fprintf(&b, "%s{namespace=%q,pod=%q,container=%q} %d\n", RestartsMetric, Namespace, BrokenPod, Container, restarts)
	fmt.Fprintf(&b, "%s{namespace=%q,pod=%q,container=%q} 0\n", RestartsMetric, Namespace, HealthyPod, Container)

	fmt.Fprintf(&b, "# HELP %s Describes the reason the container is currently in waiting state.\n", WaitingReasonMetric)
	fmt.Fprintf(&b, "# TYPE %s gauge\n", WaitingReasonMetric)
	fmt.Fprintf(&b, "%s{namespace=%q,pod=%q,container=%q,reason=\"CrashLoopBackOff\"} 1\n", WaitingReasonMetric, Namespace, BrokenPod, Container)
	fmt.Fprintf(&b, "%s{namespace=%q,pod=%q,container=%q,reason=\"ImagePullBackOff\"} 0\n", WaitingReasonMetric, Namespace, BrokenPod, Container)
	fmt.Fprintf(&b, "%s{namespace=%q,pod=%q,container=%q,reason=\"CrashLoopBackOff\"} 0\n", WaitingReasonMetric, Namespace, HealthyPod, Container)

	fmt.Fprintf(&b, "# HELP %s Describes whether the containers readiness check succeeded.\n", ReadyMetric)
	fmt.Fprintf(&b, "# TYPE %s gauge\n", ReadyMetric)
	fmt.Fprintf(&b, "%s{namespace=%q,pod=%q,container=%q} 0\n", ReadyMetric, Namespace, BrokenPod, Container)
	fmt.Fprintf(&b, "%s{namespace=%q,pod=%q,container=%q} 1\n", ReadyMetric, Namespace, HealthyPod, Container)
	return b.String()
}

// HostPort returns the host:port Prometheus should scrape.
func (w *Workload) HostPort() string {
	return strings.TrimPrefix(w.srv.URL, "http://")
}

// URL returns the workload's base URL.
func (w *Workload) URL() string { return w.srv.URL }

// Scrapes reports how many times /metrics has been served, which is also the
// broken pod's current restart count.
func (w *Workload) Scrapes() int64 { return w.restarts.Load() }

// Close shuts the metrics endpoint down.
func (w *Workload) Close() { w.srv.Close() }

// Bulk fixture identifiers. The bulk target exists only to push a real
// Prometheus past the upstream caps series_metadata sends to /api/v1/series
// and /api/v1/metadata, which the S01-shaped Workload above is far too small
// to do.
const (
	// BulkJobName is the scrape job label for a BulkWorkload target. It is
	// distinct from JobName so a bulk target and an S01 target can share a
	// server without a selector matching both.
	BulkJobName = "bloodhound-bulk"

	// BulkMetricPrefix starts every BulkWorkload metric name, so a test can
	// tell a fixture metric from one Prometheus synthesises for itself (up,
	// scrape_duration_seconds, …) without hard-coding a full name.
	BulkMetricPrefix = "bloodhound_bulk_"

	// BulkHelpPrefix starts every BulkWorkload HELP string.
	BulkHelpPrefix = "Synthetic bulk fixture metric"

	// BulkMetricType is the TYPE every BulkWorkload metric declares.
	BulkMetricType = "counter"

	// BulkShardLabel and BulkShardValue are the one label every BulkWorkload
	// series carries, so a test can check the label plumbing survived.
	BulkShardLabel = "shard"
	BulkShardValue = "a"
)

// BulkMetricName returns the name of the i-th BulkWorkload metric, counting
// from zero. The index is zero-padded so lexicographic order matches numeric
// order.
func BulkMetricName(i int) string { return fmt.Sprintf("%s%06d_total", BulkMetricPrefix, i) }

// BulkWorkload is an httptest server exposing a large number of distinct
// metric names, one series each. It exists so an integration test can make
// Prometheus's own /api/v1/series and /api/v1/metadata limits fire for real:
// the fake in mcp/prom can emit a truncation warning on demand, but only a
// real server decides when to truncate, in what order, and whether it says so.
//
// Every metric carries both HELP and TYPE. That is load-bearing: it means any
// metric that comes back from /api/v1/metadata without type or help was
// dropped by the metadata limit rather than never registered.
type BulkWorkload struct {
	srv   *httptest.Server
	body  []byte
	count int
}

// StartBulkWorkload starts a metrics endpoint exposing count distinct metric
// names on 127.0.0.1. The exposition is constant, so repeated scrapes add
// samples but no new series.
func StartBulkWorkload(count int) *BulkWorkload {
	w := &BulkWorkload{count: count, body: bulkExposition(count)}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		rw.Write(w.body) //nolint:errcheck // a failed write to a test scrape shows up as a missing series
	})
	w.srv = httptest.NewServer(mux)
	return w
}

// bulkExposition renders count metrics in the Prometheus text format, built
// once at startup because it never changes and can run to megabytes.
func bulkExposition(count int) []byte {
	var b strings.Builder
	b.Grow(count * 160)
	for i := range count {
		name := BulkMetricName(i)
		fmt.Fprintf(&b, "# HELP %s %s %d.\n", name, BulkHelpPrefix, i)
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, BulkMetricType)
		fmt.Fprintf(&b, "%s{%s=%q} %d\n", name, BulkShardLabel, BulkShardValue, i)
	}
	return []byte(b.String())
}

// Count reports how many distinct metric names the endpoint exposes, which is
// also its series count.
func (w *BulkWorkload) Count() int { return w.count }

// HostPort returns the host:port Prometheus should scrape.
func (w *BulkWorkload) HostPort() string {
	return strings.TrimPrefix(w.srv.URL, "http://")
}

// URL returns the workload's base URL.
func (w *BulkWorkload) URL() string { return w.srv.URL }

// Close shuts the metrics endpoint down.
func (w *BulkWorkload) Close() { w.srv.Close() }
