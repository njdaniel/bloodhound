// Package intake normalizes Alertmanager alerts into the facts an
// investigation starts from. In M1 the only source is a JSON file passed to
// `bloodhound hunt --alert` (spec 002 §4.1); the webhook server and
// fingerprint dedupe arrive with M2 (spec 001 §3.1).
//
// The package is deliberately free of orchestrator types: it parses and
// normalizes, and the orchestrator maps the result onto its Case. That keeps
// the dependency edge one-way and this package trivially testable.
package intake

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Alert is one normalized firing alert. Field names follow Alertmanager's
// webhook payload, which is also the shape `amtool`/`curl` dumps produce.
type Alert struct {
	// Name is the alert's "alertname" label, e.g. "KubePodCrashLooping".
	Name string `json:"name"`
	// Status is "firing" or "resolved".
	Status string `json:"status"`
	// Labels are the alert's labels, including alertname.
	Labels map[string]string `json:"labels"`
	// Annotations are the alert's annotations (summary, description, runbook).
	Annotations map[string]string `json:"annotations"`
	// StartsAt is when the alert began firing, in UTC.
	StartsAt time.Time `json:"starts_at"`
	// EndsAt is when the alert resolved; zero while it is still firing.
	EndsAt time.Time `json:"ends_at"`
	// Fingerprint is Alertmanager's dedupe key for this alert, if present.
	// M2 uses it to collapse repeated deliveries into one case.
	Fingerprint string `json:"fingerprint,omitempty"`
	// GeneratorURL links back to the rule that fired, if present.
	GeneratorURL string `json:"generator_url,omitempty"`
}

// ErrNoAlert reports that the payload parsed but carried no usable alert.
var ErrNoAlert = errors.New("intake: payload contains no alert")

// statusFiring is the Alertmanager status for an active alert.
const statusFiring = "firing"

// ParseFile reads path and parses it with Parse.
func ParseFile(path string) (Alert, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Alert{}, fmt.Errorf("reading alert file: %w", err)
	}
	a, err := Parse(data)
	if err != nil {
		return Alert{}, fmt.Errorf("parsing alert file %s: %w", path, err)
	}
	return a, nil
}

// Parse accepts either an Alertmanager webhook payload (an object with an
// "alerts" array) or a single bare alert object, and returns one normalized
// Alert. From a webhook payload it takes the first firing alert, or the first
// alert of any status when none are firing — M1 investigates one alert per
// case; grouping is M2's problem.
func Parse(data []byte) (Alert, error) {
	var payload webhookPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return Alert{}, fmt.Errorf("decoding JSON: %w", err)
	}

	raw, err := payload.pick()
	if err != nil {
		return Alert{}, err
	}
	return raw.normalize()
}

// webhookPayload is the subset of Alertmanager's webhook body M1 reads. The
// bare-alert form decodes into the same struct: an object with labels but no
// "alerts" array is an alert, not a payload.
type webhookPayload struct {
	Status string     `json:"status"`
	Alerts []rawAlert `json:"alerts"`
	rawAlert
}

// pick selects the alert to investigate from a payload or bare alert.
func (p webhookPayload) pick() (rawAlert, error) {
	if len(p.Alerts) == 0 {
		if len(p.rawAlert.Labels) == 0 {
			return rawAlert{}, ErrNoAlert
		}
		bare := p.rawAlert
		if bare.Status == "" {
			bare.Status = p.Status
		}
		return bare, nil
	}
	for _, a := range p.Alerts {
		if a.Status == statusFiring {
			return a, nil
		}
	}
	first := p.Alerts[0]
	if first.Status == "" {
		first.Status = p.Status
	}
	return first, nil
}

// rawAlert is one entry of the payload's "alerts" array, or a bare alert file.
type rawAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	Fingerprint  string            `json:"fingerprint"`
	GeneratorURL string            `json:"generatorURL"`
}

// normalize validates a raw alert and converts it to an Alert. Timestamps are
// RFC 3339 and normalized to UTC; Alertmanager's zero end time
// ("0001-01-01T00:00:00Z") becomes the zero time.
func (r rawAlert) normalize() (Alert, error) {
	name := r.Labels["alertname"]
	if name == "" {
		return Alert{}, fmt.Errorf("%w: alert has no \"alertname\" label", ErrNoAlert)
	}
	starts, err := parseTime(r.StartsAt)
	if err != nil {
		return Alert{}, fmt.Errorf("startsAt: %w", err)
	}
	ends, err := parseTime(r.EndsAt)
	if err != nil {
		return Alert{}, fmt.Errorf("endsAt: %w", err)
	}
	status := r.Status
	if status == "" {
		status = statusFiring
	}
	return Alert{
		Name:         name,
		Status:       status,
		Labels:       r.Labels,
		Annotations:  r.Annotations,
		StartsAt:     starts,
		EndsAt:       ends,
		Fingerprint:  r.Fingerprint,
		GeneratorURL: r.GeneratorURL,
	}, nil
}

// parseTime parses an RFC 3339 timestamp, treating the empty string and the
// Go zero time Alertmanager sends for unset end times as "unset".
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing %q as RFC 3339: %w", s, err)
	}
	if t.IsZero() {
		return time.Time{}, nil
	}
	return t.UTC(), nil
}
