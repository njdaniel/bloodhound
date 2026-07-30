package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// jsonFloat marshals a float using FloatFormat so goldens are byte-exact.
// NaN and ±Inf are not valid JSON numbers and are emitted as quoted strings.
type jsonFloat float64

// MarshalJSON implements json.Marshaler using FloatFormat.
func (f jsonFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	s := fmt.Sprintf(FloatFormat, v)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte(`"` + s + `"`), nil
	}
	return []byte(s), nil
}

// point is one output sample, serialized as [unix_seconds, "value"].
// The value ships pre-formatted with FloatFormat; strings dodge float JSON
// round-trip noise in goldens (spec 002 §2.3 step 5).
type point struct {
	ts  int64
	val string
}

// MarshalJSON implements json.Marshaler in the [ts, "value"] wire form.
func (p point) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`[%d,%q]`, p.ts, p.val)), nil
}

// formatValue renders a sample value with FloatFormat.
func formatValue(v float64) string {
	return fmt.Sprintf(FloatFormat, v)
}

// byteSize renders a byte count for model-facing text: exact KiB when the
// count divides evenly, plain bytes otherwise. Integer division alone would
// round a 40000-byte cap down to "39 KiB" and hand the model a number that is
// not the limit it is being told about.
func byteSize(n int) string {
	if n%1024 == 0 {
		return fmt.Sprintf("%d KiB", n/1024)
	}
	return fmt.Sprintf("%d bytes", n)
}

// seriesStats summarizes one series so the model can reason about it without
// reading every point. Always computed from full-resolution data, before any
// point-thinning. NaN samples are ignored; a series of only NaN samples
// reports zeros.
type seriesStats struct {
	Min  jsonFloat `json:"min"`
	Max  jsonFloat `json:"max"`
	Avg  jsonFloat `json:"avg"`
	Last jsonFloat `json:"last"`
}

// computeStats derives seriesStats plus the ranking inputs — value range
// (max−min) and max |value| — from full-resolution samples.
//
// Neither ranking input is ever NaN, which is what lets rankSeries be a total
// order: a NaN compares unequal to itself, so it would send the comparator
// down a branch that returns false in both directions and never reaches the
// labelset tie-break. Samples that are NaN are skipped, and the one remaining
// source — an all-+Inf or all-−Inf series, which PromQL produces readily from
// a division by zero and whose range is Inf−Inf — is normalised to −1 so it
// sorts last. handleQueryInstant normalises its own NaN |value| the same way.
func computeStats(values []promPoint) (stats seriesStats, valueRange, maxAbs float64) {
	var (
		minV, maxV, last float64
		sum              float64
		n                int
	)
	for _, p := range values {
		if math.IsNaN(p.V) {
			continue
		}
		if n == 0 {
			minV, maxV = p.V, p.V
		} else {
			minV = math.Min(minV, p.V)
			maxV = math.Max(maxV, p.V)
		}
		sum += p.V
		last = p.V
		if a := math.Abs(p.V); a > maxAbs {
			maxAbs = a
		}
		n++
	}
	if n == 0 {
		return seriesStats{}, 0, 0
	}
	valueRange = maxV - minV
	if math.IsNaN(valueRange) { // all +Inf or all −Inf; rank it last
		valueRange = -1
	}
	return seriesStats{
		Min:  jsonFloat(minV),
		Max:  jsonFloat(maxV),
		Avg:  jsonFloat(sum / float64(n)),
		Last: jsonFloat(last),
	}, valueRange, maxAbs
}

// labelsetKey renders labels in canonical sorted-key order; it is the final,
// fully deterministic tie-break for every ranking in this server. It is never
// serialized — only compared.
//
// Names and values are quoted rather than concatenated raw, because raw
// concatenation is not injective: {a: "b,c=d"} and {a: "b", c: "d"} both
// render as `a=b,c=d`. Two distinct series sharing a key make the tie-break a
// no-op, and the ranking then falls back to whatever order Prometheus
// happened to return — which is exactly the nondeterminism this key exists to
// remove. Quoting escapes the separators (and any embedded quote), so the
// rendering is reversible: distinct label sets always produce distinct keys.
func labelsetKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(k))
		b.WriteByte('=')
		b.WriteString(strconv.Quote(labels[k]))
	}
	return b.String()
}

// rankedSeries pairs a shaped output series with its ranking keys.
type rankedSeries struct {
	series     rangeSeries
	valueRange float64 // (max − min), volatility — primary key, descending
	maxAbs     float64 // max |value| — first tie-break, descending
	key        string  // canonical sorted labelset — final tie-break, ascending
}

// rankSeries sorts series per spec 002 §2.3 step 4: (max−min) descending,
// then max |value| descending, then lexicographically by sorted labelset.
//
// The comparison is a total order — so an unstable sort has exactly one
// answer to reach, whatever order the input arrived in — but only because of
// two preconditions its callers must keep:
//
//   - Distinct series produce distinct keys. Labelsets are unique within a
//     Prometheus result, and labelsetKey maps distinct labelsets to distinct
//     keys.
//   - Neither valueRange nor maxAbs is NaN. A NaN compares unequal to itself,
//     so the first two branches would both be entered and both return false,
//     ending the comparison before the key is ever read. computeStats
//     guarantees this.
//
// Break either one and the ranking silently degrades to whatever order
// Prometheus returned.
func rankSeries(rs []rankedSeries) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].valueRange != rs[j].valueRange {
			return rs[i].valueRange > rs[j].valueRange
		}
		if rs[i].maxAbs != rs[j].maxAbs {
			return rs[i].maxAbs > rs[j].maxAbs
		}
		return rs[i].key < rs[j].key
	})
}

// thinPoints drops every second interior point, always keeping the first and
// last samples (spec 002 §2.3 step 6). Series with two or fewer points are
// returned unchanged. The second return reports whether anything was dropped.
//
// The interior stride starts at index 2, and that detail is load-bearing twice
// over.
//
// It is what makes the rule converge. Output length is floor((n−2)/2)+2,
// strictly less than n for every n ≥ 3, so repeated application walks
// 33 → 17 → 9 → 5 → 3 → 2 and halts on the two-point floor the spec describes.
// A stride starting at index 1 keeps index 1, so a 3-point series thins to
// itself and reports no change: a fixed point one point above the floor, which
// stalled the query_range size backstop while a fitting payload was still
// reachable (issue #33).
//
// It also samples better than the stride it replaced. Retained indices:
//
//	n=3   0,2                  → 2 points   (old: 0,1,2 — no change)
//	n=5   0,2,4                → 3 points   (old: 0,1,3,4)
//	n=8   0,2,4,6,7            → 5 points   (old: 0,1,3,5,7)
//	n=9   0,2,4,6,8            → 5 points   (old: 0,1,3,5,7,8)
//	n=33  0,2,4,…,30,32        → 17 points  (old: 0,1,3,…,31,32)
//
// For odd n every kept index is even and the survivors are an evenly spaced
// grid; the count stays odd, so every later pass is evenly spaced too — a
// 33-point series decimates to a uniform 17, 9, 5, 3, 2. For even n the grid
// is uniform except for a single short gap at the end, forced by pinning the
// last sample. The old stride instead put its irregular gap immediately after
// the first sample and doubled up at both ends (n=9 kept 0,1 and 7,8), which
// oversampled the edges of the window at the expense of the middle.
//
// First and last are appended unconditionally and the loop's bound (i < n−1)
// keeps it from re-adding the last, so the endpoints survive every pass at
// every n — the property the model is told about in the truncation note.
func thinPoints(pts []point) ([]point, bool) {
	n := len(pts)
	if n <= 2 {
		return pts, false
	}
	out := make([]point, 0, n/2+2)
	out = append(out, pts[0])
	for i := 2; i < n-1; i += 2 {
		out = append(out, pts[i])
	}
	out = append(out, pts[n-1])
	return out, len(out) < n
}

// joinNotes assembles a truncation note from its parts, skipping empties.
//
// It copies rather than compacting parts in place. That is defensive, not a
// bug fix: every note any caller appends today is a non-empty Sprintf, so an
// in-place compaction is currently the identity. But query_range calls this
// on a slice it keeps appending to across size-backstop passes, and the day
// an empty part reaches it, compaction would quietly rewrite that slice
// between passes.
func joinNotes(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}
