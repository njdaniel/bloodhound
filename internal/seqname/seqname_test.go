package seqname

import (
	"os"
	"path/filepath"
	"testing"
)

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
		name    string
		want    int
		wantOK  bool
		filName string
	}{
		{filName: "000-hound.json", want: 0, wantOK: true, name: "zero"},
		{filName: "005-hound.json", want: 5, wantOK: true, name: "three digits"},
		{filName: "999-hound.json", want: 999, wantOK: true, name: "last three-digit value"},
		{filName: "1000-hound.json", want: 1000, wantOK: true, name: "four digits"},
		{filName: "1234567-query-range.json", want: 1234567, wantOK: true, name: "seven digits"},
		{filName: "5-hound.json", want: 5, wantOK: true, name: "one digit"},
		{filName: "-hound.json", wantOK: false, name: "no digits before the dash"},
		{filName: "abc-hound.json", wantOK: false, name: "non-digit prefix"},
		{filName: "01a-hound.json", wantOK: false, name: "digits then a letter"},
		{filName: "notes.json", wantOK: false, name: "no dash at all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Prefix(tt.filName)
			if ok != tt.wantOK {
				t.Fatalf("Prefix(%q) ok = %v, want %v", tt.filName, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("Prefix(%q) = %d, want %d", tt.filName, got, tt.want)
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
