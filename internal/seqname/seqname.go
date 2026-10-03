// Package seqname renders and parses the sequence-numbered names of capture
// files.
//
// Captures are written as NNN-<name>.json under a case's captures/ directory
// (spec 002 §4.2): llm/ by internal/llm/middleware, mcp/ by
// internal/mcpclient. Spec 002 §4.3 requires both streams to resume from
// where they stopped rather than restarting at 000 — a restart silently
// overwrites existing capture files, and captures are the evidence substrate
// Finding citations point at (spec 002 §3.4), so an overwrite destroys the
// evidence a finding cites.
//
// That shared requirement is why both halves of the name live here once
// instead of being mirrored per capture stream. Render and Prefix are two
// sides of one format, and Prefix must parse everything Render writes: a
// stream whose private renderer widened to %04d or changed the separator
// would still read its own files back, but the two streams would no longer
// agree, and disagreement's symptom is exactly the overwrite above.
package seqname

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Render returns the capture filename for sequence number seq: NNN-<name>.json,
// with name sanitized so the result is always a single path-safe filename
// component. It is the only renderer for either capture stream.
//
// The %03d is a minimum width, not a fixed one: past seq 999 names grow to
// 1000-<name>.json, which Prefix parses. For every seq in [0, math.MaxInt32]
// — the range Prefix accepts — Prefix(Render(seq, name)) returns seq, and that
// round trip is what keeps a resumed case numbering past its existing captures
// instead of on top of them.
//
// Render does not bound the length of what it returns. A caller that names
// files after an unbounded string can therefore produce a name longer than the
// filesystem's limit, which fails the write; both capture streams treat a
// capture write failure as fatal to the call. Bounding that is the caller's
// problem today, not Render's — truncating here would change every capture
// filename and needs its own collision rule.
func Render(seq int, name string) string {
	return fmt.Sprintf("%03d-%s.json", seq, sanitize(name))
}

// sanitize replaces every rune outside [A-Za-z0-9._-] with '_', so the result
// is a single filename component: no path separator survives it, on any
// platform, and neither does anything else outside that set. The mapping is
// per rune, not per byte — a multi-byte rune collapses to a single '_'.
//
// sanitize alone does not rule out "." or ".." — both are made of allowed
// runes and pass through unchanged. Render is what makes the whole filename
// safe: it prefixes at least "000-" and appends ".json", so no input can
// render to a name that traverses.
//
// Nor does it bound length: it is length-preserving in runes, and shortening a
// name is a different decision from making it path-safe (see Render).
//
// Both capture streams sanitize, on the same terms, because both name their
// files after a string the process did not author: the MCP tool name comes off
// the wire from a server, and the LLM capture label is a caller-supplied
// argument. Sanitizing here rather than at each call site is also what lets
// Render be shared at all — a renderer cannot sanitize one caller and not the
// other.
//
// The replacement is not injective, so two distinct names can sanitize to the
// same component. That is harmless: the sequence number, not the name, is what
// makes a capture filename unique.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '_' || r == '-':
			return r
		default:
			return '_'
		}
	}, name)
}

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
