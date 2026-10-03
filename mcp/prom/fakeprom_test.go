package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// fakeProm is an httptest-backed fake Prometheus serving canned /api/v1/*
// JSON. Tests override responses per path and can inspect the query
// parameters of the last request to each path.
type fakeProm struct {
	mu        sync.Mutex
	responses map[string]fakeResponse
	last      map[string]url.Values
	srv       *httptest.Server
}

// fakeResponse is one canned HTTP response.
type fakeResponse struct {
	status int
	body   string
}

// newFakeProm starts a fake Prometheus with empty-but-valid defaults for
// every endpoint the server calls.
func newFakeProm(t *testing.T) *fakeProm {
	t.Helper()
	f := &fakeProm{
		responses: map[string]fakeResponse{
			"/api/v1/query_range": {200, `{"status":"success","data":{"resultType":"matrix","result":[]}}`},
			"/api/v1/query":       {200, `{"status":"success","data":{"resultType":"vector","result":[]}}`},
			"/api/v1/alerts":      {200, `{"status":"success","data":{"alerts":[]}}`},
			"/api/v1/metadata":    {200, `{"status":"success","data":{}}`},
			"/api/v1/series":      {200, `{"status":"success","data":[]}`},
		},
		last: map[string]url.Values{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.last[r.URL.Path] = r.URL.Query()
		resp, ok := f.responses[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		fmt.Fprint(w, resp.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// url returns the fake's base URL.
func (f *fakeProm) url() string { return f.srv.URL }

// set overrides the canned response for path.
func (f *fakeProm) set(path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[path] = fakeResponse{status, body}
}

// body returns the canned response body currently set for path, so a test can
// build a variant of a fixture without restating it.
func (f *fakeProm) body(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.responses[path].body
}

// lastParams returns the query parameters of the last request to path.
func (f *fakeProm) lastParams(path string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[path]
}

// newTestToolServer wires a toolServer to the fake.
func newTestToolServer(f *fakeProm) *toolServer {
	return &toolServer{prom: newPromClient(f.url(), QueryTimeout)}
}
