package intake

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseWebhookPayload(t *testing.T) {
	const body = `{
	  "version": "4",
	  "status": "firing",
	  "groupLabels": {"alertname": "KubePodCrashLooping"},
	  "alerts": [
	    {
	      "status": "resolved",
	      "labels": {"alertname": "OldAlert"},
	      "startsAt": "2026-07-28T09:00:00Z"
	    },
	    {
	      "status": "firing",
	      "labels": {"alertname": "KubePodCrashLooping", "namespace": "shop", "pod": "checkout-7d9"},
	      "annotations": {"summary": "Pod is restarting"},
	      "startsAt": "2026-07-28T10:02:00+00:00",
	      "endsAt": "0001-01-01T00:00:00Z",
	      "fingerprint": "a1b2c3d4",
	      "generatorURL": "http://prom/graph?g0.expr=up"
	    }
	  ]
	}`

	got, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Name != "KubePodCrashLooping" {
		t.Errorf("name = %q, want the firing alert's, not the first entry's", got.Name)
	}
	if got.Status != statusFiring {
		t.Errorf("status = %q, want %q", got.Status, statusFiring)
	}
	if got.Labels["pod"] != "checkout-7d9" {
		t.Errorf("labels = %v, want the pod label carried through", got.Labels)
	}
	if got.Annotations["summary"] != "Pod is restarting" {
		t.Errorf("annotations = %v", got.Annotations)
	}
	want := time.Date(2026, 7, 28, 10, 2, 0, 0, time.UTC)
	if !got.StartsAt.Equal(want) {
		t.Errorf("startsAt = %s, want %s", got.StartsAt, want)
	}
	if got.StartsAt.Location() != time.UTC {
		t.Errorf("startsAt location = %s, want UTC", got.StartsAt.Location())
	}
	if !got.EndsAt.IsZero() {
		t.Errorf("endsAt = %s, want the zero time for a firing alert", got.EndsAt)
	}
	if got.Fingerprint != "a1b2c3d4" || got.GeneratorURL == "" {
		t.Errorf("fingerprint/generatorURL = %q/%q", got.Fingerprint, got.GeneratorURL)
	}
}

func TestParseBareAlert(t *testing.T) {
	const body = `{
	  "labels": {"alertname": "HighErrorRate", "job": "checkout"},
	  "startsAt": "2026-07-28T10:02:00Z"
	}`
	got, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Name != "HighErrorRate" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Status != statusFiring {
		t.Errorf("status = %q, want the firing default", got.Status)
	}
}

func TestParseFallsBackToTheFirstAlertWhenNoneAreFiring(t *testing.T) {
	const body = `{"status":"resolved","alerts":[{"status":"resolved","labels":{"alertname":"Gone"}}]}`
	got, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Name != "Gone" || got.Status != "resolved" {
		t.Errorf("alert = %+v, want the single resolved alert", got)
	}
}

func TestParseRejectsUnusablePayloads(t *testing.T) {
	tests := map[string]string{
		"not json":         `{`,
		"no alerts":        `{"version":"4","alerts":[]}`,
		"empty object":     `{}`,
		"no alertname":     `{"alerts":[{"labels":{"severity":"page"}}]}`,
		"bad startsAt":     `{"alerts":[{"labels":{"alertname":"X"},"startsAt":"yesterday"}]}`,
		"bad endsAt":       `{"alerts":[{"labels":{"alertname":"X"},"endsAt":"soon"}]}`,
		"alerts not array": `{"alerts":"nope"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
		})
	}
}

func TestParseNoAlertIsSentinel(t *testing.T) {
	if _, err := Parse([]byte(`{"alerts":[]}`)); !errors.Is(err, ErrNoAlert) {
		t.Fatalf("error = %v, want ErrNoAlert", err)
	}
}

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alert.json")
	if err := os.WriteFile(path, []byte(`{"labels":{"alertname":"X"}}`), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if _, err := ParseFile(path); err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	_, err := ParseFile(filepath.Join(dir, "missing.json"))
	if err == nil {
		t.Fatal("ParseFile succeeded on a missing file")
	}
	if !strings.Contains(err.Error(), "reading alert file") {
		t.Errorf("error = %v, want it to name the read failure", err)
	}
}
