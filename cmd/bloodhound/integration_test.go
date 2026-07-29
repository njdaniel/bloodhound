//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/llm/llmtest"
	"github.com/njdaniel/bloodhound/internal/orchestrator"
	"github.com/njdaniel/bloodhound/internal/promtest"
)

// This is the demo path of docs/demo-m1.md run as a test: the real CLI, the
// real orchestrator, the real mcp-prom binary, and a real Prometheus
// container scraping a real /metrics endpoint. The only substituted component
// is the model — a scripted llmtest.Provider stands in for Anthropic, so the
// suite needs no API key and makes no paid call (CLAUDE.md).
//
// What that buys over the httptest-fake e2e test in hunt_test.go: the
// evidence in the resulting report cites captures of genuine Prometheus
// responses, so `capture_ref` resolution is exercised against real data
// rather than against a canned fixture.

// DemoWorkEnv names an environment variable that, when set, is used as the
// work root instead of a temp dir — so a run of this test leaves behind the
// case directory that docs/demo-m1/ ships as its artifact.
const DemoWorkEnv = "BLOODHOUND_DEMO_WORK"

// demoAlert is the Alertmanager payload the demo investigates. It matches the
// S01 "bad image tag → CrashLoopBackOff" eval scenario
// (evals/scenarios/S01-bad-image-tag) and the fixture metrics promtest serves.
const demoAlert = `{
  "version": "4",
  "status": "firing",
  "receiver": "bloodhound",
  "groupLabels": {"alertname": "KubePodCrashLooping"},
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "KubePodCrashLooping",
        "namespace": "shop",
        "pod": "checkout-7d9f",
        "container": "checkout",
        "severity": "critical"
      },
      "annotations": {
        "summary": "Pod shop/checkout-7d9f is restarting repeatedly",
        "description": "checkout has restarted more than 5 times in the last 10 minutes"
      },
      "startsAt": "2026-07-28T10:05:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "fingerprint": "a1b2c3d4e5f60718"
    }
  ]
}`

// startDemoStack brings up the fixture metrics endpoint and a Prometheus
// container scraping it, skipping the test when docker is unavailable.
func startDemoStack(t *testing.T) *promtest.Server {
	t.Helper()
	if err := promtest.CheckDocker(t.Context()); err != nil {
		t.Skipf("skipping real-Prometheus CLI demo: %v\n"+
			"Install docker and re-run `make test-integration`; `make check` does not need it.", err)
	}
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
	ready := fmt.Sprintf("count_over_time(%s{pod=%q}[5m]) >= 40", promtest.RestartsMetric, promtest.BrokenPod)
	if err := srv.WaitForSamples(t.Context(), ready, 1); err != nil {
		logs, _ := srv.Logs(t.Context())
		t.Fatalf("no samples landed: %v\ncontainer logs:\n%s", err, logs)
	}
	return srv
}

// scriptedInvestigation is the model side of the demo: discover the metric
// surface, pull the restart counter over the live window, then submit a
// finding citing the second tool call. The queries are real and run against
// the real server; only the decision to make them is canned.
func scriptedInvestigation(start, end time.Time) []llm.Response {
	toolUse := func(id, name, input string) llm.Response {
		return llm.Response{
			Message: llm.AssistantMessage(llm.ToolUseBlock(llm.ToolUse{
				ID: id, Name: name, Input: json.RawMessage(input),
			})),
			StopReason: llm.StopToolUse,
			Usage:      llm.Usage{InputTokens: 1500, OutputTokens: 120},
		}
	}
	discover := fmt.Sprintf(`{"match": %q}`, `{namespace="shop"}`)
	restarts := fmt.Sprintf(`{"query": %q, "start": %q, "end": %q}`,
		fmt.Sprintf(`%s{namespace="shop",pod="%s"}`, promtest.RestartsMetric, promtest.BrokenPod),
		start.Format(time.RFC3339), end.Format(time.RFC3339))
	finding := fmt.Sprintf(`{
      "hound": "metrics",
      "summary": "checkout-7d9f is in CrashLoopBackOff and its restart counter is still climbing across the alert window; the container never reaches a ready state, which is the signature of a container that fails immediately on start rather than one degrading under load.",
      "confidence": 0.78,
      "onset": %q,
      "evidence": [
        {
          "tool": "query_range",
          "query": %q,
          "observation": "the restart counter rises monotonically across the window and has not plateaued",
          "tool_call_index": 2
        }
      ],
      "dead_ends": [
        {
          "theory": "a traffic spike overloaded checkout",
          "ruled_out_by": "no request-rate series exists in this namespace's metric surface, so the theory cannot be supported from metrics alone"
        }
      ]
    }`,
		start.Format(time.RFC3339),
		fmt.Sprintf(`%s{namespace="shop",pod="%s"}`, promtest.RestartsMetric, promtest.BrokenPod))

	return []llm.Response{
		toolUse("t1", "series_metadata", discover),
		toolUse("t2", "query_range", restarts),
		{
			Message: llm.AssistantMessage(llm.ToolUseBlock(llm.ToolUse{
				ID: "t3", Name: "submit_finding", Input: json.RawMessage(finding),
			})),
			StopReason: llm.StopToolUse,
			Usage:      llm.Usage{InputTokens: 4100, OutputTokens: 380},
		},
	}
}

// TestHuntAgainstRealPrometheus runs `bloodhound hunt --alert` end to end
// against a live Prometheus and asserts that the finding's evidence resolves
// to a capture of a genuine query result — the acceptance criterion issue #6
// states as "the demo report cites real captured queries".
func TestHuntAgainstRealPrometheus(t *testing.T) {
	srv := startDemoStack(t)

	work := os.Getenv(DemoWorkEnv)
	if work == "" {
		work = t.TempDir()
	} else if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("creating %s work root: %v", DemoWorkEnv, err)
	}
	alert := filepath.Join(t.TempDir(), "alert.json")
	if err := os.WriteFile(alert, []byte(demoAlert), 0o644); err != nil {
		t.Fatalf("writing alert: %v", err)
	}

	// A two-minute window clamps to a 1s step, so the ten seconds of scraped
	// data startDemoStack waited for come back as a readable handful of
	// points rather than as two.
	end := time.Now().UTC()
	start := end.Add(-2 * time.Minute)
	p := llmtest.New(t, scriptedInvestigation(start, end)...)
	a, stdout, stderr := newTestApp(t, p, nil)
	a.now = time.Now

	if err := a.run(t.Context(), []string{
		"hunt",
		"--alert", alert,
		"--work", work,
		"--model", "scripted-demo",
		"--mcp-prom", buildMCPProm(t),
		"--prom-url", srv.URL,
	}); err != nil {
		t.Fatalf("hunt: %v\nstderr: %s", err, stderr)
	}

	dirs := caseDirs(t, work)
	if len(dirs) != 1 {
		t.Fatalf("case dirs = %v, want exactly one", dirs)
	}
	caseDir := dirs[0]
	t.Logf("case work dir: %s", caseDir)
	t.Logf("terminal report:\n%s", stdout)

	// The finding's evidence must point at a capture file that actually
	// exists and actually holds the Prometheus response for that query.
	var report struct {
		Evidence []struct {
			Tool       string `json:"tool"`
			Query      string `json:"query"`
			CaptureRef string `json:"capture_ref"`
		} `json:"evidence"`
	}
	raw, err := os.ReadFile(filepath.Join(caseDir, orchestrator.ReportJSON))
	if err != nil {
		t.Fatalf("reading report.json: %v", err)
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decoding report.json: %v", err)
	}
	if len(report.Evidence) != 1 {
		t.Fatalf("evidence entries = %d, want 1", len(report.Evidence))
	}
	ev := report.Evidence[0]
	if ev.CaptureRef == "" {
		t.Fatal("evidence has no capture_ref; the loop must inject one")
	}
	capture, err := os.ReadFile(filepath.Join(caseDir, orchestrator.CaptureDir, ev.CaptureRef))
	if err != nil {
		t.Fatalf("evidence capture_ref %q does not resolve: %v", ev.CaptureRef, err)
	}
	var captured struct {
		Tool   string `json:"tool"`
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(capture, &captured); err != nil {
		t.Fatalf("decoding capture %s: %v", ev.CaptureRef, err)
	}
	if captured.Tool != ev.Tool {
		t.Errorf("capture %s is for tool %q, but the evidence cites %q", ev.CaptureRef, captured.Tool, ev.Tool)
	}
	// The tool result must hold real scrape data. job and instance are labels
	// Prometheus itself attaches to a scraped series — the fixture exposition
	// never emits them — so their presence proves the capture came off the
	// wire rather than out of a canned response.
	body := captured.Result.Text
	for _, want := range []string{promtest.RestartsMetric, promtest.BrokenPod, promtest.JobName, `"instance"`} {
		if !strings.Contains(body, want) {
			t.Errorf("captured tool result does not contain %s; it should hold a real Prometheus response:\n%s", want, body)
		}
	}

	// Both MCP calls were captured in order, and cost accounting saw all
	// three scripted model turns.
	mcp := captureNames(t, caseDir, "mcp")
	if len(mcp) != 2 || mcp[0] != "000-series_metadata.json" || mcp[1] != "001-query_range.json" {
		t.Errorf("mcp captures = %v, want the two tool calls in order", mcp)
	}
	c, checkpoints, total, err := orchestrator.Cost(work, filepath.Base(caseDir))
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if c.Phase != orchestrator.PhaseDone {
		t.Errorf("case phase = %q, want %q", c.Phase, orchestrator.PhaseDone)
	}
	if len(checkpoints) != 3 {
		t.Errorf("checkpoints = %d, want 3", len(checkpoints))
	}
	if total.ToolCalls != 2 {
		t.Errorf("tool calls = %d, want 2", total.ToolCalls)
	}
	// The scripted provider is priced at zero: no paid call was made, and a
	// cost report must not invent a number it did not spend.
	if total.USD != 0 {
		t.Errorf("total USD = %v, want 0 for a scripted provider", total.USD)
	}
}
