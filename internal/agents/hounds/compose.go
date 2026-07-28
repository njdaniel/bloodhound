package hounds

import (
	"fmt"

	"github.com/njdaniel/bloodhound/internal/llm"
	"github.com/njdaniel/bloodhound/internal/llm/middleware"
)

// Compose builds the provider stack a hound run needs, in the order
// internal/llm/middleware documents:
//
//	retry → accounting → capture → raw provider
//
// Capture is innermost so each file records exactly what went over the wire,
// one per attempt. Accounting sits inside retry so it sees every attempt's
// usage — an accounting layer placed outside retry counts a retried call once
// and undercounts Spend. Retry is outermost so a retried call is captured and
// accounted like any other.
//
// captureDir is the case work dir's captures/ directory; the capture
// decorator continues its sequence numbering from whatever is already there,
// so a resumed case does not restart at 000 (spec 002 §4.3). label names the
// caller in capture filenames and must be path-safe.
//
// The two return values are the two Deps fields that must agree: pass them
// straight into Deps.Provider and Deps.Accounting. Wiring the stack anywhere
// else risks getting the order wrong; that is the point of this function.
func Compose(p llm.Provider, captureDir, label string, retry middleware.RetryConfig) (llm.Provider, *middleware.Accounting, error) {
	if p == nil {
		return nil, nil, fmt.Errorf("hounds: Compose needs a provider")
	}
	if captureDir == "" {
		return nil, nil, fmt.Errorf("hounds: Compose needs a capture dir")
	}
	if label == "" {
		return nil, nil, fmt.Errorf("hounds: Compose needs a capture label")
	}
	capture, err := middleware.NewCapture(p, captureDir, label)
	if err != nil {
		return nil, nil, fmt.Errorf("hounds: composing capture middleware: %w", err)
	}
	acct := middleware.NewAccounting(capture)
	return middleware.NewRetry(acct, retry), acct, nil
}

// MetricsLabel is the capture label for metrics-hound's model calls: capture
// files are named captures/llm/NNN-metrics-hound.json.
const MetricsLabel = "metrics-hound"
