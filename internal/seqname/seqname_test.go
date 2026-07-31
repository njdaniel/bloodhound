package seqname

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderMatchesTheOnDiskFormat pins the exact filename both capture
// streams write. It is the format's one definition, so a change here is a
// change to every capture file's name and to what a resumed case reads back.
func TestRenderMatchesTheOnDiskFormat(t *testing.T) {
	tests := []struct {
		name string
		seq  int
		in   string
		want string
	}{
		{"llm label", 0, "metrics-hound", "000-metrics-hound.json"},
		{"mcp tool", 0, "query_range", "000-query_range.json"},
		{"zero padded to three", 5, "hound", "005-hound.json"},
		{"last three-digit value", 999, "echo", "999-echo.json"},
		// #10: %03d is a minimum width. Past 999 the field grows rather than
		// truncating or wrapping, and Prefix reads the wider field back.
		{"widens past three digits", 1000, "hound", "1000-hound.json"},
		{"seven digits", 1234567, "query-range", "1234567-query-range.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Render(tt.seq, tt.in); got != tt.want {
				t.Errorf("Render(%d, %q) = %q, want %q", tt.seq, tt.in, got, tt.want)
			}
		})
	}
}

// TestRenderRoundTripsThroughPrefix is the invariant that binds the two halves
// of this package: Prefix must read back exactly what Render wrote. Both
// capture streams call Render and resume via Next, which calls Prefix, so a
// renderer/parser disagreement is a resumed case numbering on top of its own
// existing captures (spec 002 §4.3).
func TestRenderRoundTripsThroughPrefix(t *testing.T) {
	// The corpus spans both ends of the range Render documents, [0,
	// math.MaxInt32]: 0 and math.MaxInt32 itself are the boundaries Prefix
	// accepts, and the widths in between are #10's territory.
	for _, seq := range []int{0, 1, 5, 99, 100, 998, 999, 1000, 1001, 12345, 2147483646, math.MaxInt32} {
		for _, name := range []string{"hound", "metrics-hound", "echo", "query_range", "../../evil", ""} {
			got, ok := Prefix(Render(seq, name))
			if !ok {
				t.Errorf("Prefix(Render(%d, %q) = %q) rejected the name it rendered", seq, name, Render(seq, name))
				continue
			}
			if got != seq {
				t.Errorf("Prefix(Render(%d, %q)) = %d, want %d", seq, name, got, seq)
			}
		}
	}
}

// TestSanitizeStripsPathSyntax checks the rule the MCP tool name has always
// been held to and that the LLM capture label is now held to as well: nothing
// outside [A-Za-z0-9._-] survives, so no rendered name can carry a separator.
func TestSanitizeStripsPathSyntax(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"leaves a real label alone", "metrics-hound", "metrics-hound"},
		{"leaves a real tool alone", "query_range", "query_range"},
		{"parent traversal", "../evil", ".._evil"},
		{"deep traversal", "../../etc/passwd", ".._.._etc_passwd"},
		{"absolute path", "/etc/passwd", "_etc_passwd"},
		{"backslash separator", `..\..\evil`, ".._.._evil"},
		{"null byte", "ev\x00il", "ev_il"},
		{"newline", "ev\nil", "ev_il"},
		{"spaces and quotes", `a b"c`, "a_b_c"},
		// Per rune, not per byte: 'ü' is two bytes and collapses to one '_'.
		{"non-ascii", "hoünd", "ho_nd"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitize(tt.in)
			if got != tt.want {
				t.Errorf("sanitize(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.ContainsAny(got, `/\`) {
				t.Errorf("sanitize(%q) = %q, which still contains a path separator", tt.in, got)
			}
		})
	}
}

// TestRenderCannotTraverse is the property the table above only samples: for
// any name at all, the rendered filename is a single component that stays put.
// sanitize alone does not rule out "." or ".." — Render's numeric prefix and
// .json suffix are what do.
func TestRenderCannotTraverse(t *testing.T) {
	for _, name := range []string{
		"..", ".", "../evil", "../../../../etc/passwd", "/etc/passwd",
		`..\..\evil`, "a/b", "", "\x00", "....//....//evil",
	} {
		got := Render(0, name)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("Render(0, %q) = %q, which contains a path separator", name, got)
		}
		if got == "." || got == ".." {
			t.Errorf("Render(0, %q) = %q, which is a directory reference", name, got)
		}
		if filepath.Base(got) != got {
			t.Errorf("Render(0, %q) = %q, which is not a single path component", name, got)
		}
		// The decisive check: joining it onto a directory must land inside.
		const dir = "/work/case/captures/mcp"
		if joined := filepath.Join(dir, got); filepath.Dir(joined) != dir {
			t.Errorf("Render(0, %q) joined onto %q gives %q, which escapes", name, dir, joined)
		}
	}
}

// TestNextParsesSequencePrefix is the single table for both capture streams.
// It folds together what used to be two byte-identical tables — one in
// internal/llm/middleware (label fixtures: "hound", "metrics-hound"), one in
// internal/mcpclient (tool fixtures: "echo", "query-range"). The two tables
// were 1:1 isomorphic: no case existed in only one of them. Where the two
// copies used different fixture values for the same case, both are kept here.
func TestNextParsesSequencePrefix(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  int
	}{
		{"empty dir", nil, 0},
		// Both copies' three-digit seeds, which differed only in value.
		{"three digits", []string{"005-hound.json"}, 6},
		{"three digits, other seed", []string{"007-echo.json"}, 8},
		// #10: %03d is a minimum width, so numbering must survive past 999.
		// A fixed-width parser skips these and restarts into a collision.
		{"four digits", []string{"1000-hound.json"}, 1001},
		{"mixed widths", []string{"999-hound.json", "1234-hound.json"}, 1235},
		// Both copies' dash-in-the-name fixtures: the prefix ends at the
		// first '-', so a name that itself contains dashes still parses.
		{"label contains a dash", []string{"012-metrics-hound.json"}, 13},
		{"tool name contains a dash", []string{"012-query-range.json"}, 13},
		{"no sequence prefix", []string{"notes.json", "-hound.json", "abc-hound.json"}, 0},
		{"prefix beyond the bound", []string{"10000000000-hound.json"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
					t.Fatalf("seeding %s: %v", name, err)
				}
			}
			got, err := Next(dir)
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if got != tt.want {
				t.Errorf("Next = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestPrefixParsesVariableWidth pins #10's variable-width behaviour directly
// on the parser, not only through Next: the prefix is every digit up to the
// first '-', at any width, and three digits are not special.
func TestPrefixParsesVariableWidth(t *testing.T) {
	tests := []struct {
		name     string
		want     int
		wantOK   bool
		fileName string
	}{
		{fileName: "000-hound.json", want: 0, wantOK: true, name: "zero"},
		{fileName: "005-hound.json", want: 5, wantOK: true, name: "three digits"},
		{fileName: "999-hound.json", want: 999, wantOK: true, name: "last three-digit value"},
		{fileName: "1000-hound.json", want: 1000, wantOK: true, name: "four digits"},
		{fileName: "1234567-query-range.json", want: 1234567, wantOK: true, name: "seven digits"},
		{fileName: "5-hound.json", want: 5, wantOK: true, name: "one digit"},
		{fileName: "-hound.json", wantOK: false, name: "no digits before the dash"},
		{fileName: "abc-hound.json", wantOK: false, name: "non-digit prefix"},
		{fileName: "01a-hound.json", wantOK: false, name: "digits then a letter"},
		{fileName: "notes.json", wantOK: false, name: "no dash at all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Prefix(tt.fileName)
			if ok != tt.wantOK {
				t.Fatalf("Prefix(%q) ok = %v, want %v", tt.fileName, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("Prefix(%q) = %d, want %d", tt.fileName, got, tt.want)
			}
		})
	}
}

// TestPrefixRejectsOverflowingValues guards the one way the widened parser
// could be worse than the fixed-width one it replaced: a corrupt or
// hand-planted name whose digits parse but whose n+1 overflows, wrapping
// negative so Next ignores it and restarts numbering over live captures.
// The parse is bounded so that value is unrepresentable.
func TestPrefixRejectsOverflowingValues(t *testing.T) {
	for _, name := range []string{
		"9223372036854775807-hound.json", // math.MaxInt64: n+1 wraps to MinInt64
		"9223372036854775807-echo.json",
		"10000000000-hound.json", // 1e10: parses on 64-bit, far past any real case
		"10000000000-echo.json",
		"2147483648-hound.json", // math.MaxInt32+1: the first rejected value
	} {
		if n, ok := Prefix(name); ok {
			t.Errorf("Prefix(%q) = %d, true; want rejected (bounded parse)", name, n)
		}
	}
	// The bound is inclusive of math.MaxInt32 itself.
	if n, ok := Prefix("2147483647-hound.json"); !ok || n != 2147483647 {
		t.Errorf("Prefix(math.MaxInt32) = %d, %v; want 2147483647, true", n, ok)
	}
	// And through Next: a planted name must not steer live numbering. Both
	// capture streams' fixtures from the two folded tables.
	for _, tc := range []struct {
		files []string
		want  int
	}{
		{[]string{"9223372036854775807-hound.json", "005-hound.json"}, 6},
		{[]string{"9223372036854775807-echo.json", "007-echo.json"}, 8},
	} {
		dir := t.TempDir()
		for _, name := range tc.files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
				t.Fatalf("seeding %s: %v", name, err)
			}
		}
		got, err := Next(dir)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if got != tc.want {
			t.Errorf("Next = %d, want %d (the planted name is ignored)", got, tc.want)
		}
	}
}

// TestNextReportsMissingDir checks the wrapped error path: a capture dir that
// cannot be read must surface, not silently number from 0 and overwrite.
func TestNextReportsMissingDir(t *testing.T) {
	if _, err := Next(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("Next on a missing dir returned nil error; want an error")
	}
}
