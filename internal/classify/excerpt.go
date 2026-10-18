package classify

import (
	"bytes"
	"slices"
	"strings"
	"unicode"
)

// Excerpt limits (docs/design.md §5.3: at most 40 lines in a result capped at 4 KB).
const (
	MaxExcerptLines   = 40
	MaxExcerptLineLen = 200 // runes, including the "…" of a cut line
	MaxExcerptBytes   = 2048
)

// errorMarkers pick the lines an excerpt is built from.
var errorMarkers = [][]byte{
	[]byte("error:"), []byte("ERROR"), []byte("FAILED"), []byte("ITMS-"), []byte("✘"),
	[]byte("Error Domain="), []byte("Step failed"), []byte("fatal error"), []byte("Unable to"),
	[]byte("BSErrorCodeDescription"), []byte("NSLocalizedFailureReason"), []byte("' failed"),
	[]byte("Failing tests:"),
}

// Excerpt returns the most relevant lines of log, at most max (MaxExcerptLines when max is out
// of range) and MaxExcerptBytes in total: error, FAILED and ITMS lines, each with up to three
// following note: or indented continuation lines, in log order, cleaned (see cleanLine),
// deduplicated and cut to MaxExcerptLineLen runes. A log without such lines yields its last ten
// non-empty lines.
func Excerpt(log []byte, max int) []string {
	if max <= 0 || max > MaxExcerptLines {
		max = MaxExcerptLines
	}
	type pick struct {
		at      int
		text    string
		primary bool
	}
	var picks []pick
	seen := map[string]bool{}
	add := func(at int, text string, primary bool) {
		text = truncate(text, MaxExcerptLineLen)
		if text == "" || seen[text] {
			return
		}
		seen[text] = true
		picks = append(picks, pick{at, text, primary})
	}

	lines := bytes.Split(log, []byte{'\n'})
	for i, l := range lines {
		if !isErrorLine(l) {
			continue
		}
		text := cleanLine(l)
		if isArt(text) {
			continue
		}
		add(i, text, true)
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			c := cleanLine(lines[j])
			if c == "" || isArt(c) {
				continue
			}
			if isErrorLine(lines[j]) || !strings.HasPrefix(c, "note:") && !isIndented(lines[j]) {
				break
			}
			add(j, c, false)
		}
	}
	if len(picks) == 0 {
		for i := len(lines) - 1; i >= 0 && len(picks) < min(10, max); i-- {
			if c := cleanLine(lines[i]); c != "" && !isArt(c) {
				add(i, c, true)
			}
		}
		slices.Reverse(picks)
	}

	if len(picks) > max {
		// Keep error lines before context; among error lines, the first ones and the last
		// (usually "** … FAILED **").
		var primary, context []pick
		for _, p := range picks {
			if p.primary {
				primary = append(primary, p)
			} else {
				context = append(context, p)
			}
		}
		if len(primary) >= max {
			last := primary[len(primary)-1]
			primary = primary[:max]
			if max > 1 {
				primary[max-1] = last
			}
			context = nil
		} else {
			context = context[:max-len(primary)]
		}
		picks = append(primary, context...)
		slices.SortFunc(picks, func(a, b pick) int { return a.at - b.at })
	}

	out := make([]string, 0, len(picks))
	total := 0
	for _, p := range picks {
		if len(out) > 0 && total+len(p.text) > MaxExcerptBytes {
			break
		}
		total += len(p.text)
		out = append(out, p.text)
	}
	return out
}

func isErrorLine(l []byte) bool {
	for _, m := range errorMarkers {
		if bytes.Contains(l, m) {
			return true
		}
	}
	return false
}

// isArt reports Swift's diagnostic drawing ("|   `- error: …"), which repeats the error line,
// and separator lines such as devicectl's "-----…".
func isArt(s string) bool {
	if strings.HasPrefix(s, "|") || strings.HasPrefix(s, "`-") {
		return true
	}
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0
}

func isIndented(l []byte) bool {
	return len(l) > 0 && (l[0] == ' ' || l[0] == '\t')
}
