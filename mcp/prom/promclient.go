package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// promClient is a minimal Prometheus HTTP API client built on net/http and
// encoding/json. The query API is small and stable; using the stdlib directly
// avoids a client_golang dependency (stdlib-first rule, spec 002 §2).
type promClient struct {
	baseURL string
	timeout time.Duration
	hc      *http.Client
}

// newPromClient returns a client for the Prometheus instance at baseURL,
// applying timeout as the context deadline on every call.
func newPromClient(baseURL string, timeout time.Duration) *promClient {
	return &promClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		timeout: timeout,
		hc:      &http.Client{},
	}
}

// apiEnvelope is the standard Prometheus API response wrapper.
//
// Warnings and Infos are the two severities of the non-fatal annotations
// Prometheus attaches to an otherwise successful response. They are separate
// arrays on the wire and stay separate here: a warning says the result may be
// wrong ("bucket label \"le\" is missing or has a malformed value"), an info
// says the expression is suspect but the result is what was asked for
// ("metric might not be a counter, name does not end in _total/…"). Collapsing
// them would throw away the only severity signal the server gives.
//
// Warnings also carries "results truncated due to limit", which is the only
// signal that a server-side cap took effect on /api/v1/series.
type apiEnvelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	Warnings  []string        `json:"warnings"`
	Infos     []string        `json:"infos"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
}

// promAnnotations carries the non-fatal notices Prometheus attached to a
// successful response, at the two severities it distinguishes.
type promAnnotations struct {
	// Warnings mean the result itself may be wrong.
	Warnings []string
	// Infos mean the expression is suspect though the result is well-defined.
	Infos []string
}

// empty reports whether Prometheus said nothing at all.
func (a promAnnotations) empty() bool { return len(a.Warnings) == 0 && len(a.Infos) == 0 }

// get performs a GET against path with params and returns the "data" payload,
// discarding any annotations.
//
// It is only for endpoints that cannot produce them. Annotations come out of
// PromQL evaluation, so the endpoints that never evaluate an expression —
// /api/v1/alerts, which returns the rule manager's active alert list, and
// /api/v1/metadata, which returns scrape-time type and help text — have
// nothing to attach. Measured against v3.5.0: neither response carries a
// warnings or infos key at all, and the integration suite pins that
// (TestUpstreamAnnotationsAgainstRealPrometheus/silent_endpoints). Every
// endpoint that does evaluate PromQL must use getAnnotated instead.
func (c *promClient) get(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	data, _, err := c.getAnnotated(ctx, path, params)
	return data, err
}

// getAnnotated performs a GET against path with params, enforcing the client
// timeout, and returns the "data" payload plus the response's annotations.
// Prometheus API errors (bad PromQL, invalid parameters) are returned as
// errors carrying the upstream message so the model can correct its input.
func (c *promClient) getAnnotated(ctx context.Context, path string, params url.Values) (json.RawMessage, promAnnotations, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var none promAnnotations
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, none, fmt.Errorf("building request for %s: %w", path, err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, none, fmt.Errorf("calling prometheus %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, none, fmt.Errorf("reading prometheus %s response: %w", path, err)
	}
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, none, fmt.Errorf("decoding prometheus %s response (HTTP %d): %w", path, resp.StatusCode, err)
	}
	if env.Status != "success" {
		return nil, none, fmt.Errorf("prometheus rejected the request (%s): %s — fix the query and retry", env.ErrorType, env.Error)
	}
	return env.Data, promAnnotations{Warnings: env.Warnings, Infos: env.Infos}, nil
}

// promPoint is one Prometheus sample: a float timestamp (unix seconds) and a
// string-encoded value.
type promPoint struct {
	T float64
	V float64
}

// UnmarshalJSON decodes the Prometheus wire form [ts, "value"].
func (p *promPoint) UnmarshalJSON(b []byte) error {
	var pair [2]json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil {
		return fmt.Errorf("decoding sample pair: %w", err)
	}
	if err := json.Unmarshal(pair[0], &p.T); err != nil {
		return fmt.Errorf("decoding sample timestamp: %w", err)
	}
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil {
		return fmt.Errorf("decoding sample value: %w", err)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("parsing sample value %q: %w", s, err)
	}
	p.V = v
	return nil
}

// promSeries is one matrix result: a labelset and its samples.
type promSeries struct {
	Metric map[string]string `json:"metric"`
	Values []promPoint       `json:"values"`
}

// queryRange calls /api/v1/query_range and returns the matrix result together
// with the annotations Prometheus attached to the evaluation.
func (c *promClient) queryRange(ctx context.Context, query string, start, end time.Time, step int64) ([]promSeries, promAnnotations, error) {
	params := url.Values{
		"query": {query},
		"start": {formatTime(start)},
		"end":   {formatTime(end)},
		"step":  {strconv.FormatInt(step, 10)},
	}
	data, ann, err := c.getAnnotated(ctx, "/api/v1/query_range", params)
	if err != nil {
		return nil, promAnnotations{}, err
	}
	var d struct {
		ResultType string       `json:"resultType"`
		Result     []promSeries `json:"result"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, promAnnotations{}, fmt.Errorf("decoding query_range data: %w", err)
	}
	if d.ResultType != "matrix" {
		return nil, promAnnotations{}, fmt.Errorf("query_range returned result type %q, expected matrix", d.ResultType)
	}
	return d.Result, ann, nil
}

// promSample is one instant-vector sample.
type promSample struct {
	Metric map[string]string `json:"metric"`
	Value  promPoint         `json:"value"`
}

// query calls /api/v1/query and returns the result type ("vector" or
// "scalar") with the samples, plus the annotations Prometheus attached to the
// evaluation; a scalar comes back as one sample with an empty labelset. ts may
// be zero to use the server's current time.
func (c *promClient) query(ctx context.Context, query string, ts time.Time) (string, []promSample, promAnnotations, error) {
	params := url.Values{"query": {query}}
	if !ts.IsZero() {
		params.Set("time", formatTime(ts))
	}
	data, ann, err := c.getAnnotated(ctx, "/api/v1/query", params)
	if err != nil {
		return "", nil, promAnnotations{}, err
	}
	var d struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return "", nil, promAnnotations{}, fmt.Errorf("decoding query data: %w", err)
	}
	switch d.ResultType {
	case "vector":
		var samples []promSample
		if err := json.Unmarshal(d.Result, &samples); err != nil {
			return "", nil, promAnnotations{}, fmt.Errorf("decoding vector result: %w", err)
		}
		return d.ResultType, samples, ann, nil
	case "scalar":
		var p promPoint
		if err := json.Unmarshal(d.Result, &p); err != nil {
			return "", nil, promAnnotations{}, fmt.Errorf("decoding scalar result: %w", err)
		}
		return d.ResultType, []promSample{{Metric: map[string]string{}, Value: p}}, ann, nil
	default:
		return "", nil, promAnnotations{}, fmt.Errorf("query returned result type %q; use query_range for matrix results", d.ResultType)
	}
}

// promAlert is one alert from /api/v1/alerts.
type promAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
	ActiveAt    string            `json:"activeAt"`
	Value       string            `json:"value"`
}

// alerts calls /api/v1/alerts and returns every active alert (both pending
// and firing); the caller filters by state.
func (c *promClient) alerts(ctx context.Context) ([]promAlert, error) {
	data, err := c.get(ctx, "/api/v1/alerts", nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Alerts []promAlert `json:"alerts"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("decoding alerts data: %w", err)
	}
	return d.Alerts, nil
}

// promMetadata is one metadata entry from /api/v1/metadata.
type promMetadata struct {
	Type string `json:"type"`
	Help string `json:"help"`
	Unit string `json:"unit"`
}

// metadata calls /api/v1/metadata and returns metadata entries keyed by
// metric name. limit caps the number of metrics the server is asked for;
// Prometheus versions that predate the parameter ignore it, so callers must
// still cap what they use.
func (c *promClient) metadata(ctx context.Context, limit int) (map[string][]promMetadata, error) {
	data, err := c.get(ctx, "/api/v1/metadata", url.Values{"limit": {strconv.Itoa(limit)}})
	if err != nil {
		return nil, err
	}
	var d map[string][]promMetadata
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("decoding metadata data: %w", err)
	}
	return d, nil
}

// series calls /api/v1/series with the given series selector, restricted to
// the [start, end] window, and returns the matching labelsets together with
// the response's warnings. Without a window Prometheus scans the full
// retention period. limit caps the number of series the server is asked for;
// versions that predate the parameter ignore it, so callers must still cap
// what they use — and must read the warnings rather than the result count to
// learn whether the server actually truncated.
//
// Only the warnings are returned: /api/v1/series does not evaluate PromQL, so
// the infos half of the envelope is always empty for it.
func (c *promClient) series(ctx context.Context, match string, start, end time.Time, limit int) ([]map[string]string, []string, error) {
	data, ann, err := c.getAnnotated(ctx, "/api/v1/series", url.Values{
		"match[]": {match},
		"start":   {formatTime(start)},
		"end":     {formatTime(end)},
		"limit":   {strconv.Itoa(limit)},
	})
	if err != nil {
		return nil, nil, err
	}
	var sets []map[string]string
	if err := json.Unmarshal(data, &sets); err != nil {
		return nil, nil, fmt.Errorf("decoding series data: %w", err)
	}
	return sets, ann.Warnings, nil
}

// formatTime renders t as unix seconds (fractional if needed) for Prometheus
// query parameters.
func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', -1, 64)
}
