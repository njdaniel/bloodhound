// Package middleware provides composable decorators around llm.Provider:
// retry with exponential backoff on transient API errors, token/cost
// accounting, and request/response capture to the case work dir.
//
// Intended composition order, outermost first:
//
//	retry → accounting → capture → provider
//
//	capture, err := middleware.NewCapture(provider, workDir, "metrics-hound")
//	acct := middleware.NewAccounting(capture)
//	p := middleware.NewRetry(acct, middleware.RetryConfig{})
//
// Capture is innermost so each capture file records exactly what went over
// the wire — one file per attempt, including attempts that failed and were
// retried. Accounting sits inside retry so it observes every retry attempt's
// usage and cost, not just the final outcome. Retry is outermost so a retried
// call is re-captured and re-accounted like any other attempt.
package middleware
