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
	return seriesStats{
		Min:  jsonFloat(minV),
		Max:  jsonFloat(maxV),
		Avg:  jsonFloat(sum / float64(n)),
		Last: jsonFloat(last),
	}, maxV - minV, maxAbs
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
// Labelsets are unique within a Prometheus result and labelsetKey maps
// distinct labelsets to distinct keys, so the comparison is a total order:
// an unstable sort has exactly one answer to arrive at, whatever order the
// input arrived in.
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
func thinPoints(pts []point) ([]point, bool) {
	n := len(pts)
	if n <= 2 {
		return pts, false
	}
	out := make([]point, 0, n/2+2)
	out = append(out, pts[0])
	for i := 1; i < n-1; i += 2 {
		out = append(out, pts[i])
	}
	out = append(out, pts[n-1])
	return out, len(out) < n
}

// joinNotes assembles a truncation note from its parts, skipping empties.
// It does not modify parts: query_range calls it on a note slice it keeps
// appending to across size-backstop passes, and compacting in place would
// rewrite that caller's slice under it.
func joinNotes(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}
