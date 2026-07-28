package hounds

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// Run investigates a case with metrics-hound: an LLM tool-use loop whose
// tools are one MCP server's tools (mcp-prom in M1, fetched live from the
// session so the server's descriptions are the single source of truth) plus
// the synthetic submit_finding tool it terminates by calling.
//
// focus states the questions this hound should answer; in M1 it is derived
// mechanically from the alert with MetricsFocus, and the planner takes over
// authorship in M2. budget caps tool calls, tokens, and wall clock; zero
// fields take the package defaults.
//
// Run returns the submitted Finding with capture refs resolved from the tool
// calls the model cited, and the Spend the run cost. It returns an error if
// the context ends, the model or MCP session fails, or the model cannot
// produce a schema-valid finding within MaxRepairAttempts repairs.
func Run(ctx context.Context, deps Deps, c orchestrator.Case, focus string, budget orchestrator.Budget) (orchestrator.Finding, orchestrator.Spend, error) {
	cfg := loopConfig{
		hound:        MetricsHound,
		systemPrompt: MetricsSystemPrompt(),
		deps:         deps,
		budget:       budget,
	}
	finding, spend, err := runLoop(ctx, cfg, openingTurn(c, focus))
	if err != nil {
		return orchestrator.Finding{}, spend, err
	}
	return finding, spend, nil
}

// MetricsFocus derives M1's focus questions mechanically from the alert: no
// model call, no planner, just the alert's own facts turned into the three
// questions metrics-hound exists to answer (spec 001 §3.1). M2's planner
// replaces this with authored focus.
func MetricsFocus(c orchestrator.Case) string {
	var b strings.Builder
	fmt.Fprintf(&b, "1. When did the condition behind %s begin? Establish the onset time.\n", quoteOrPlaceholder(c.AlertName, "this alert"))
	b.WriteString("2. What do the metrics for the alerting workload show around that onset — error rates, saturation, restarts, latency?\n")
	b.WriteString("3. What else moved at the same time, and what plausible causes can you rule out?\n")
	if scope := labelScope(c.Labels); scope != "" {
		fmt.Fprintf(&b, "\nStart from the alert's own selectors (%s) and widen only if they come back empty.", scope)
	}
	return b.String()
}

// openingTurn renders the user turn that opens a hound run: the case facts,
// then the focus questions.
func openingTurn(c orchestrator.Case, focus string) string {
	var b strings.Builder
	b.WriteString("# Case\n\n")
	fmt.Fprintf(&b, "case_id: %s\n", c.ID)
	fmt.Fprintf(&b, "alert: %s\n", c.AlertName)
	if !c.FiringSince.IsZero() {
		fmt.Fprintf(&b, "firing_since: %s\n", c.FiringSince.UTC().Format(time.RFC3339))
	}
	if len(c.Labels) > 0 {
		b.WriteString("labels:\n")
		for _, k := range sortedKeys(c.Labels) {
			fmt.Fprintf(&b, "  %s: %s\n", k, c.Labels[k])
		}
	}
	b.WriteString("\n# Focus\n\n")
	b.WriteString(strings.TrimRight(focus, "\n"))
	b.WriteString("\n")
	return b.String()
}

// labelScope renders the alert labels that make good PromQL selectors, in a
// stable order, or "" if the alert carries none of them.
func labelScope(labels map[string]string) string {
	// Ordered by how tightly each narrows a query, not alphabetically.
	interesting := []string{"namespace", "pod", "container", "job", "instance", "service", "deployment"}
	var parts []string
	for _, k := range interesting {
		if v, ok := labels[k]; ok && v != "" {
			parts = append(parts, fmt.Sprintf("%s=%q", k, v))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

// quoteOrPlaceholder quotes s, or returns fallback when s is empty.
func quoteOrPlaceholder(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return fmt.Sprintf("%q", s)
}
