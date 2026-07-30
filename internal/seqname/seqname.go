// Package seqname parses the sequence-number prefix of capture filenames.
//
// Captures are written as NNN-<name>.json under a case's captures/ directory
// (spec 002 §4.2): llm/ by internal/llm/middleware, mcp/ by
// internal/mcpclient. Spec 002 §4.3 requires both streams to resume from
// where they stopped rather than restarting at 000 — a restart silently
// overwrites existing capture files, and captures are the evidence substrate
// Finding citations point at (spec 002 §3.4), so an overwrite destroys the
// evidence a finding cites.
//
// That shared requirement is why the numbering lives here once instead of
// being mirrored per capture stream: two copies can drift, and the drift
// symptom is exactly the overwrite the requirement exists to prevent.
package seqname

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Next returns one past the highest sequence number already present in dir,
// or 0 for an empty directory. Names without a sequence prefix are ignored.
func Next(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("reading dir: %w", err)
	}
	next := 0
	for _, e := range entries {
		n, ok := Prefix(e.Name())
		if !ok {
			continue
		}
		if n+1 > next {
			next = n + 1
		}
	}
	return next, nil
}

// Prefix parses the leading run of digits before the first '-' in a capture
// filename. It reports false for any name that does not start with at least
// one digit followed by '-', and for values above math.MaxInt32.
//
// The prefix is every digit up to the first '-', not a fixed three: %03d is a
// minimum width, so past seq 999 filenames grow to 1000-<name>.json and a
// fixed-width parser would skip them and restart numbering into a collision.
//
// The upper bound keeps Next's n+1 from overflowing on a corrupt or
// hand-planted name, which would wrap negative and silently restart numbering
// on top of existing captures. ~2e9 is headroom no real case will approach.
func Prefix(name string) (int, bool) {
	i := strings.IndexByte(name, '-')
	if i <= 0 {
		return 0, false
	}
	digits := name[:i]
	for j := 0; j < len(digits); j++ {
		if digits[j] < '0' || digits[j] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(digits, 10, 32)
	if err != nil {
		return 0, false
	}
	return int(n), true
}
