package classify

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExcerptSelectsErrorsWithContext(t *testing.T) {
	log := strings.Join([]string{
		"CompileSwift normal arm64 Foo.swift",
		"2026-10-13 20:36:07.387 xcodebuild[49310:31102936] Progress 0 %: Preparing",
		"error: exportArchive Cloud signing permission error",
		"  You haven't been given access to cloud-managed distribution certificates.",
		"unrelated line",
		"Foo.swift:3:1: error: expected expression",
		"    |   `- error: expected expression", // Swift's drawing of the same error
		"note: did you mean 'bar'?",
		"error: exportArchive Cloud signing permission error", // duplicate
		"\x1b[1;31merror: \x1b[0mcoloured",
		"2026-10-13 20:38:44.086 xcodebuild[50096:31106020] Progress 0 %: Upload failed. ERROR",
		"** EXPORT FAILED **",
	}, "\n")
	want := []string{
		"error: exportArchive Cloud signing permission error",
		"You haven't been given access to cloud-managed distribution certificates.",
		"Foo.swift:3:1: error: expected expression",
		"note: did you mean 'bar'?",
		"error: coloured",
		"Progress 0 %: Upload failed. ERROR",
		"** EXPORT FAILED **",
	}
	if got := Excerpt([]byte(log), 40); !slices.Equal(got, want) {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

func TestExcerptFallsBackToTail(t *testing.T) {
	var b strings.Builder
	for i := range 30 {
		fmt.Fprintf(&b, "line %d\n\n", i)
	}
	got := Excerpt([]byte(b.String()), 40)
	if len(got) != 10 || got[0] != "line 20" || got[9] != "line 29" {
		t.Fatalf("tail excerpt %q", got)
	}
	if got := Excerpt(nil, 40); len(got) != 0 {
		t.Fatalf("empty log: %q", got)
	}
}

func TestExcerptBounds(t *testing.T) {
	var b strings.Builder
	for i := range 500 {
		fmt.Fprintf(&b, "F%d.swift:%d:1: error: e%d\n", i, i, i)
		if i%3 == 0 {
			fmt.Fprintf(&b, "    note: context %d\n", i)
		}
	}
	b.WriteString("** BUILD FAILED **\n")
	log := []byte(b.String())
	for _, max := range []int{-1, 0, 1, 5, 40, 41, 1000} {
		got := Excerpt(log, max)
		limit := max
		if max <= 0 || max > MaxExcerptLines {
			limit = MaxExcerptLines
		}
		if len(got) == 0 || len(got) > limit {
			t.Fatalf("max %d: %d lines", max, len(got))
		}
		if size(got) > MaxExcerptBytes {
			t.Fatalf("max %d: %d bytes", max, size(got))
		}
		if got[0] != "F0.swift:0:1: error: e0" {
			t.Fatalf("max %d: first line %q", max, got[0])
		}
	}
	// With room for few lines, the final verdict line survives over later errors.
	if got := Excerpt(log, 3); got[2] != "** BUILD FAILED **" {
		t.Fatalf("max 3: %q", got)
	}
}

func TestExcerptLongAndUnicodeLines(t *testing.T) {
	long := "error: " + strings.Repeat("é✘", 200_000) // ~1 MB, multi-byte runes
	invalid := "error: bad \xff\xfe bytes"
	got := Excerpt([]byte(long+"\n"+invalid+"\n"), 40)
	if len(got) != 2 {
		t.Fatalf("got %d lines", len(got))
	}
	for _, l := range got {
		if !utf8.ValidString(l) || utf8.RuneCountInString(l) > MaxExcerptLineLen {
			t.Fatalf("bad line (%d runes, valid %v)", utf8.RuneCountInString(l), utf8.ValidString(l))
		}
	}
	if !strings.HasSuffix(got[0], "…") || utf8.RuneCountInString(got[0]) != MaxExcerptLineLen {
		t.Fatalf("long line not cut to %d runes with an ellipsis", MaxExcerptLineLen)
	}
	if got[1] != "error: bad � bytes" {
		t.Fatalf("invalid UTF-8 line %q", got[1])
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"abcdef", 4, "abc…"},
		{"äöüß", 3, "äö…"},
		{"x", 1, "x"},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestCleanLine(t *testing.T) {
	cases := map[string]string{
		"2026-10-14 13:12:57.566 xcodebuild[29780:32557969] Progress 0 %: x": "Progress 0 %: x",
		"2026-10-14 16:28:31 +0000 [MT] Step failed: y":                      "Step failed: y",
		"2026-10-14 16:28:31 +0000  Step failed: z":                          "Step failed: z",
		"\t  \x1b[1;31merror:\x1b[0m w  ":                                    "error: w",
		"2026 is a year":                                                     "2026 is a year",
	}
	for in, want := range cases {
		if got := cleanLine([]byte(in)); got != want {
			t.Errorf("cleanLine(%q) = %q, want %q", in, got, want)
		}
	}
}
