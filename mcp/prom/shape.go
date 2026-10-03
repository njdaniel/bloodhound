package main

import (
	"fmt"
	"math"
	"slices"
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
// One pass on an odd n keeps only even indices, so the survivors are an evenly
// spaced grid. Whether the whole chain stays uniform is a narrower claim: an
// odd n = 2k+1 retains k+1 points, which is odd again only when n ≡ 1 (mod 4),
// so the uniform 33 → 17 → 9 → 5 → 3 → 2 above is the n = 2^j+1 family, not
// odd n in general — n=7 goes 0,2,4,6 (uniform) then 0,4,6, and n=11 reaches
// gaps of 8,2 by its third pass. The canonical input is in that second case,
// since MaxPointsPerSeries and effectiveStep clamp a full-window query to 121
// points:
//
//	121 → 61 → 31 → 16 → 9 → 5 → 3 → 2
//	gaps:  2…   4…   8…   16×7,8   32×3,24   64,56   120
//
// So the grid is exactly uniform for the first three passes and then carries
// one odd gap. That gap is bounded and lands where it hurts least: it is
// always the final one and always shorter than the rest, so the newest samples
// are never thinned harder than the window average — which for incident work
// is the end you want intact. The old stride put its irregular gaps at both
// ends (n=9 kept 0,1 and 7,8), spending extra points on the start of the
// window, and on n=3 it kept everything.
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

// queryAnnotationsOut is the model-facing block carrying the PromQL
// annotations Prometheus attached to a query_range or query_instant result.
// It is emitted only when the server said something (see shapeAnnotations),
// so its presence is itself the signal.
//
// The two severities stay in separate arrays, mirroring the wire format,
// because they call for different actions: a warning means the numbers in this
// very payload may be meaningless and must not be reported without a caveat,
// while an info means the expression is a likely mistake even though the
// result is well-defined. Flattening into one list would leave the model to
// re-derive that from the "PromQL warning:"/"PromQL info:" prefixes — a
// prefix the server is under no obligation to keep.
//
// The counts are named *_total to match the truncation blocks elsewhere in
// this package. There is deliberately no matching *_returned: those blocks
// need one because the thing they count (series, samples) lives at the top
// level of the payload, whereas here the kept list is the adjacent field, so
// a _returned would only ever restate its length. Compare each total to its
// array to see whether anything was dropped.
type queryAnnotationsOut struct {
	// Warnings are the kept warnings: the result may be wrong.
	Warnings []string `json:"warnings,omitempty"`
	// WarningsTotal is how many distinct warnings the server sent.
	WarningsTotal int `json:"warnings_total"`
	// Infos are the kept infos: the expression is suspect.
	Infos []string `json:"infos,omitempty"`
	// InfosTotal is how many distinct infos the server sent.
	InfosTotal int `json:"infos_total"`
	// Note names every cap this block applied, or is empty if none did.
	Note string `json:"note,omitempty"`
}

// shapeAnnotations bounds the annotations Prometheus attached to a query
// response and returns the block to embed, or nil when the server said
// nothing.
//
// Unlike series_metadata's warning handling, nothing here is classified into a
// bloodhound-authored sentence. That split is deliberate and consistent:
// series_metadata classifies "results truncated due to limit" because it
// describes a cap *bloodhound asked for* (it sends limit=MaxUpstreamSeries),
// so it can be restated as advice the model can act on, and passes every other
// warning verbatim. query_range and query_instant send no limit at all, so no
// annotation they receive is about a bloodhound cap — every one is a PromQL
// diagnostic whose value is its exact wording, naming the metric and the
// position in the expression. Restating those would delete the only part worth
// having. So: everything verbatim, everything bounded.
//
// Ordering the input before capping is load-bearing rather than cosmetic.
// Prometheus accumulates annotations in a map, so the order it serializes them
// in is its map iteration order — identical calls to one server return the
// same annotations in different orders, which the integration test measures
// and logs on every run (TestUpstreamAnnotationsAgainstRealPrometheus/wire).
// Keeping the first MaxQueryAnnotations of *that* would hand the model a
// different subset on every call and make any assertion on the output flaky.
// Which subset is kept is decided by pickAcrossKinds; see there for why plain
// alphabetical order is not it.
//
// Compaction after sorting is defensive: the wire arrays are map keys and so
// already distinct, but if that ever stops being true the cap should spend its
// slots on distinct diagnostics rather than repeats.
func shapeAnnotations(ann promAnnotations) *queryAnnotationsOut {
	if ann.empty() {
		return nil
	}
	warnings, warningsTotal := capAnnotations(ann.Warnings)
	infos, infosTotal := capAnnotations(ann.Infos)

	// One sentence per applied cap, in the order of the block's own fields,
	// then the advice they share appended once — the shape series_metadata's
	// note uses.
	var notes []string
	if dropped := warningsTotal - len(warnings); dropped > 0 {
		notes = append(notes, fmt.Sprintf("%d further warnings dropped; kept one of each distinct kind first, then alphabetically.", dropped))
	}
	if dropped := infosTotal - len(infos); dropped > 0 {
		notes = append(notes, fmt.Sprintf("%d further infos dropped; kept one of each distinct kind first, then alphabetically.", dropped))
	}
	if len(notes) > 0 {
		notes = append(notes, "Prometheus raises most annotations once per affected metric, so a broad selector produces near-identical repeats; narrow it to see the rest.")
	}
	return &queryAnnotationsOut{
		Warnings:      warnings,
		WarningsTotal: warningsTotal,
		Infos:         infos,
		InfosTotal:    infosTotal,
		Note:          joinNotes(notes...),
	}
}

// capAnnotations sorts, de-duplicates and caps one severity's annotations at
// MaxQueryAnnotations, truncating each to MaxQueryAnnotationLen. It returns
// the kept strings and how many distinct ones there were. It copies rather
// than sorting in place so the caller's slice — decoded straight out of the
// HTTP response — is not reordered underneath it.
func capAnnotations(in []string) ([]string, int) {
	if len(in) == 0 {
		return nil, 0
	}
	all := slices.Clone(in)
	slices.Sort(all)
	all = slices.Compact(all)
	total := len(all)
	if len(all) > MaxQueryAnnotations {
		all = pickAcrossKinds(all, MaxQueryAnnotations)
	}
	out := make([]string, 0, len(all))
	for _, s := range all {
		out = append(out, truncateString(s, MaxQueryAnnotationLen))
	}
	return out, total
}

// annotationKind returns the grouping key for an annotation: the message up to
// its first quoted operand or numeric literal.
//
// Prometheus builds annotations from a fixed set of templates and fills each
// with the operand it is complaining about — a metric name in quotes, or a
// number. Everything before that first operand is the template, so it is what
// distinguishes "this is the bucket-label problem" from "this is the quantile
// problem", while the eight bucket-label warnings one broad selector raises
// collapse to a single key:
//
//	bucket label "le" is missing … for metric name "up" (1:25)   → `…bucket label `
//	quantile value should be between 0 and 1, got 1.5 (1:20)     → `…should be between `
//
// A heuristic, deliberately. Getting it wrong only changes which subset of a
// capped list is kept — never a message's content, never determinism — and the
// failure mode is mild in both directions: splitting one template into two
// kinds costs a slot, merging two into one costs the guarantee below for that
// pair.
func annotationKind(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '"' || (c >= '0' && c <= '9') {
			return s[:i]
		}
	}
	return s
}

// pickAcrossKinds selects n annotations from sorted (already sorted and
// de-duplicated) by taking one per kind in turn before taking a second of any,
// then returns the selection in sorted order.
//
// Plain alphabetical truncation is what this replaces, and it was actively
// harmful rather than merely arbitrary. Prometheus raises the per-metric
// annotations once per affected metric, and their template — "bucket label …"
// — starts with a `b`, while essentially every other template starts later in
// the alphabet ("encountered a mix …", "invalid quantile …", "quantile value
// should be …", "vector contains …"). So sorting put the near-identical
// repeats first *every time*, and the cap dropped the one warning that said
// something new. Measured against v3.5.0: `histogram_quantile(1.5, {job="…"})`
// returns nine warnings — eight bucket-label repeats and one "quantile value
// should be between 0 and 1, got 1.5" — and the five alphabetically first are
// all bucket-label. A model told only that would fix the metric type and
// repeat the 1.5-should-be-0.95 mistake on its next query.
//
// Round-robin gives the property worth having: while there are no more kinds
// than slots, every kind is represented. The repeats still get the leftover
// slots, since one example of a kind is worth much more than its second.
//
// Determinism is preserved end to end. The input is sorted and distinct, kinds
// are visited in first-seen (so alphabetical-by-kind) order, each kind's
// members are consumed in sorted order, and the result is sorted again before
// return — so the output is a function of the *set* of annotations, which is
// what the upstream map iteration order makes necessary.
func pickAcrossKinds(sorted []string, n int) []string {
	kinds := make([]string, 0, len(sorted))
	groups := make(map[string][]string, len(sorted))
	for _, s := range sorted {
		k := annotationKind(s)
		if _, seen := groups[k]; !seen {
			kinds = append(kinds, k)
		}
		groups[k] = append(groups[k], s)
	}

	kept := make([]string, 0, n)
	for round := 0; len(kept) < n; round++ {
		progressed := false
		for _, k := range kinds {
			g := groups[k]
			if round >= len(g) {
				continue
			}
			kept = append(kept, g[round])
			progressed = true
			if len(kept) == n {
				break
			}
		}
		if !progressed { // every kind exhausted; nothing left to take
			break
		}
	}
	slices.Sort(kept)
	return kept
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
