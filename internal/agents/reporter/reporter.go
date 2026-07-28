// Package reporter renders a finished case into the two artifacts the M1
// report phase produces: report.json for machines and report.txt for the
// terminal (spec 002 §4.1). Slack delivery is M3.
//
// Rendering is pure and deterministic — same input, same bytes — so the
// artifacts can be golden-tested and diffed across runs. The orchestrator, not
// this package, writes the files, so every work dir write goes through one
// atomic path.
package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// SchemaVersion is the version stamped into report.json. Like the checkpoint
// schema it guards migrations: readers refuse versions they do not know.
const SchemaVersion = 1

// Report is the schema of report.json. It is the case's public result:
// everything a reader needs to judge the hypothesis without opening the work
// dir, plus the capture refs to check it if they want to.
type Report struct {
	// SchemaVersion is SchemaVersion.
	SchemaVersion int `json:"schema_version"`
	// CaseID identifies the case, and its work dir under the work root.
	CaseID string `json:"case_id"`
	// AlertName and Labels are the alert the case was opened from.
	AlertName string            `json:"alert_name"`
	Labels    map[string]string `json:"labels,omitempty"`
	// FiringSince is when the alert started firing.
	FiringSince time.Time `json:"firing_since,omitzero"`
	// GeneratedAt is when the report was rendered.
	GeneratedAt time.Time `json:"generated_at"`
	// Hound names the investigator behind the hypothesis, e.g. "metrics".
	Hound string `json:"hound"`
	// Hypothesis is the hound's summary: what it thinks happened.
	Hypothesis string `json:"hypothesis"`
	// Confidence is the hound's self-assessed confidence, 0..1.
	Confidence float64 `json:"confidence"`
	// Onset is when the anomaly began, if the hound determined it.
	Onset *time.Time `json:"onset,omitempty"`
	// Evidence is what the hound observed, each anchored to a capture file.
	Evidence []orchestrator.Evidence `json:"evidence"`
	// DeadEnds is what the hound ruled out. They are first-class output:
	// they are what makes the hypothesis trustworthy.
	DeadEnds []orchestrator.DeadEnd `json:"dead_ends"`
	// Spend is what the case cost.
	Spend orchestrator.Spend `json:"spend"`
}

// Render implements orchestrator.ReportFunc: it builds the Report and returns
// report.json and report.txt as artifacts for the orchestrator to write.
func Render(_ context.Context, in orchestrator.ReportInput) ([]orchestrator.Artifact, error) {
	report := New(in)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling report: %w", err)
	}
	return []orchestrator.Artifact{
		{Name: orchestrator.ReportJSON, Data: append(data, '\n')},
		{Name: orchestrator.ReportText, Data: []byte(Text(report))},
	}, nil
}

// Render satisfies the orchestrator's report hook; this pins the contract so a
// signature change there fails the build here rather than at wiring time.
var _ orchestrator.ReportFunc = Render

// New builds the Report for a finished case.
func New(in orchestrator.ReportInput) Report {
	return Report{
		SchemaVersion: SchemaVersion,
		CaseID:        in.Case.ID,
		AlertName:     in.Case.AlertName,
		Labels:        in.Case.Labels,
		FiringSince:   in.Case.FiringSince,
		GeneratedAt:   in.GeneratedAt.UTC(),
		Hound:         in.Finding.Hound,
		Hypothesis:    in.Finding.Summary,
		Confidence:    in.Finding.Confidence,
		Onset:         in.Finding.Onset,
		Evidence:      in.Finding.Evidence,
		DeadEnds:      in.Finding.DeadEnds,
		Spend:         in.Spend,
	}
}

// Terminal layout constants. The report is plain ASCII with no color: it is
// read in terminals, in CI logs, and as a checked-in artifact, and the same
// bytes must serve all three.
const (
	// reportWidth is the target line width for rules and wrapped prose.
	reportWidth = 78
	// colQuery and colObservation cap the evidence table's wide columns.
	colTool        = 14
	colQuery       = 30
	colObservation = 28
)

// Text renders the terminal report: hypothesis, confidence, evidence table,
// dead ends, and a spend footer.
func Text(r Report) string {
	var b strings.Builder

	fmt.Fprintf(&b, "bloodhound report — case %s\n", r.CaseID)
	b.WriteString(rule('=') + "\n")
	fmt.Fprintf(&b, "alert:   %s\n", orPlaceholder(r.AlertName, "(unnamed)"))
	if !r.FiringSince.IsZero() {
		fmt.Fprintf(&b, "firing:  %s\n", r.FiringSince.UTC().Format(time.RFC3339))
	}
	if scope := labelLine(r.Labels); scope != "" {
		fmt.Fprintf(&b, "scope:   %s\n", scope)
	}
	fmt.Fprintf(&b, "hound:   %s\n", orPlaceholder(r.Hound, "(unknown)"))

	fmt.Fprintf(&b, "\nHYPOTHESIS  (confidence %.2f)\n", r.Confidence)
	if r.Onset != nil {
		fmt.Fprintf(&b, "onset %s\n", r.Onset.UTC().Format(time.RFC3339))
	}
	b.WriteString(wrap(orPlaceholder(r.Hypothesis, "(no summary)"), reportWidth) + "\n")

	b.WriteString("\nEVIDENCE\n")
	b.WriteString(evidenceTable(r.Evidence))

	b.WriteString("\nDEAD ENDS\n")
	if len(r.DeadEnds) == 0 {
		b.WriteString("  (none recorded)\n")
	}
	for _, d := range r.DeadEnds {
		b.WriteString(bullet("  - ", d.Theory))
		b.WriteString(bullet("    ruled out by: ", d.RuledOutBy))
	}

	b.WriteString("\n" + rule('-') + "\n")
	fmt.Fprintf(&b, "spend: %d in + %d out tokens · $%.4f · %d tool calls · %s\n",
		r.Spend.InputTokens, r.Spend.OutputTokens, r.Spend.USD, r.Spend.ToolCalls,
		(time.Duration(r.Spend.WallMS) * time.Millisecond).Round(time.Millisecond))
	return b.String()
}

// evidenceTable renders the evidence records as a fixed-width table. Long
// cells are elided rather than wrapped so rows stay one line each; the capture
// ref is never truncated, because it is the pointer to the full record.
func evidenceTable(ev []orchestrator.Evidence) string {
	if len(ev) == 0 {
		return "  (none recorded)\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %-3s %-*s %-*s %-*s %s\n", "#",
		colTool, "TOOL", colQuery, "QUERY", colObservation, "OBSERVED", "CAPTURE")
	for i, e := range ev {
		fmt.Fprintf(&b, "  %-3d %-*s %-*s %-*s %s\n", i+1,
			colTool, elide(e.Tool, colTool),
			colQuery, elide(e.Query, colQuery),
			colObservation, elide(e.Observation, colObservation),
			e.CaptureRef)
	}
	return b.String()
}

// labelLine renders the labels that best identify the workload, in a fixed
// order, falling back to every label sorted by key.
func labelLine(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	preferred := []string{"namespace", "pod", "container", "job", "instance", "service", "deployment", "severity"}
	var parts []string
	for _, k := range preferred {
		if v := labels[k]; v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k != "alertname" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, " ")
}

// elide shortens s to width runes, ending with an ellipsis when it had to cut.
func elide(s string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	runes := []rune(s)
	if width <= 1 {
		return string(runes[:width])
	}
	return string(runes[:width-1]) + "…"
}

// wrap breaks s into lines of at most width runes on word boundaries. Words
// longer than width are left intact rather than split mid-token: a truncated
// PromQL expression is worse than a long line.
func wrap(s string, width int) string {
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		line := words[0]
		for _, w := range words[1:] {
			if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(w) > width {
				lines = append(lines, line)
				line = w
				continue
			}
			line += " " + w
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// bullet renders prefix followed by text, wrapped to the report width with
// continuation lines aligned under the first one.
func bullet(prefix, text string) string {
	pad := strings.Repeat(" ", utf8.RuneCountInString(prefix))
	var b strings.Builder
	for i, line := range strings.Split(wrap(text, reportWidth-utf8.RuneCountInString(prefix)), "\n") {
		if i == 0 {
			b.WriteString(prefix)
		} else {
			b.WriteString(pad)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// rule renders a full-width horizontal rule.
func rule(c byte) string { return strings.Repeat(string(c), reportWidth) }

// orPlaceholder returns s, or fallback when s is empty.
func orPlaceholder(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
