package source

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strings"
)

// Ignore is an ordered set of gitignore-style patterns. Patterns from deeper directories are
// added after their ancestors', so "the last matching pattern wins" gives git's precedence.
//
// Supported: blank lines and # comments, \# and \! escapes, ! negation, a leading or middle /
// anchoring the pattern to its directory, a trailing / matching directories only, *, ?,
// [...] classes (including [!...] and POSIX classes), and ** as a whole segment (leading,
// trailing or between slashes).
//
// Not supported: escaped trailing spaces ("foo\ "), case-insensitive matching (core.ignorecase),
// and git's global sources (core.excludesFile, .git/info/exclude); in a git work tree git itself
// applies those.
type Ignore struct {
	rules []rule
}

type rule struct {
	base     string // slash-separated directory the pattern is relative to; "" is the root
	re       *regexp.Regexp
	negate   bool
	dirOnly  bool
	anchored bool // false: re matches the last path segment only
	text     string
}

// ParseIgnore returns an Ignore holding patterns relative to base ("" for the root).
func ParseIgnore(base string, patterns []string) *Ignore {
	ig := &Ignore{}
	ig.Add(base, patterns)
	return ig
}

// Add appends patterns relative to the slash-separated directory base.
func (ig *Ignore) Add(base string, patterns []string) {
	base = strings.Trim(base, "/")
	for _, p := range patterns {
		if r, ok := compileRule(base, p); ok {
			ig.rules = append(ig.rules, r)
		}
	}
}

// AddFile appends the patterns of an ignore file (such as .gitignore) whose directory is base. A
// missing file is not an error.
func (ig *Ignore) AddFile(base, path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	lines, err := readLines(f)
	if err != nil {
		return err
	}
	ig.Add(base, lines)
	return nil
}

func readLines(r io.Reader) ([]string, error) {
	var lines []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

// Empty reports whether ig holds no patterns.
func (ig *Ignore) Empty() bool { return ig == nil || len(ig.rules) == 0 }

// Match reports whether the slash-separated path rel (relative to the root) is ignored by the
// patterns themselves, without considering its parent directories.
func (ig *Ignore) Match(rel string, isDir bool) bool {
	if ig == nil {
		return false
	}
	ignored := false
	name := rel[strings.LastIndexByte(rel, '/')+1:]
	for _, r := range ig.rules {
		if r.dirOnly && !isDir {
			continue
		}
		sub, ok := under(rel, r.base)
		if !ok {
			continue
		}
		if r.anchored && r.re.MatchString(sub) || !r.anchored && r.re.MatchString(name) {
			ignored = !r.negate
		}
	}
	return ignored
}

// Ignored reports whether rel is excluded, either directly or because one of its parent
// directories is. As in git, a file cannot be re-included once its directory is excluded.
func (ig *Ignore) Ignored(rel string, isDir bool) bool {
	if ig.Empty() {
		return false
	}
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' && ig.Match(rel[:i], true) {
			return true
		}
	}
	return ig.Match(rel, isDir)
}

// under returns rel relative to base when rel lies strictly inside base.
func under(rel, base string) (string, bool) {
	if base == "" {
		return rel, true
	}
	if strings.HasPrefix(rel, base+"/") {
		return rel[len(base)+1:], true
	}
	return "", false
}

func compileRule(base, line string) (rule, bool) {
	line = strings.TrimRight(line, " \t\r")
	if line == "" || strings.HasPrefix(line, "#") {
		return rule{}, false
	}
	r := rule{base: base, text: line}
	switch {
	case strings.HasPrefix(line, "!"):
		r.negate = true
		line = line[1:]
	case strings.HasPrefix(line, `\!`), strings.HasPrefix(line, `\#`):
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return rule{}, false
	}
	// A slash anywhere but the end anchors the pattern to its directory.
	r.anchored = strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	re, err := regexp.Compile("^" + globRegexp(line) + "$")
	if err != nil {
		return rule{}, false
	}
	r.re = re
	return r, true
}

// globRegexp translates one gitignore glob into a regular expression body.
func globRegexp(g string) string {
	var b strings.Builder
	segs := strings.Split(g, "/")
	for i, seg := range segs {
		last := i == len(segs)-1
		if seg == "**" {
			switch {
			case last && i == 0:
				b.WriteString(".*")
			case last:
				// "a/**" matches everything inside a, not a itself; the separator is already written.
				b.WriteString(".+")
			default:
				b.WriteString("(?:.*/)?")
			}
			continue
		}
		b.WriteString(segRegexp(seg))
		if !last {
			b.WriteByte('/')
		}
	}
	return b.String()
}

func segRegexp(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteString(regexp.QuoteMeta(string(s[i])))
			}
		case '[':
			end := classEnd(s, i)
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := s[i+1 : end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// classEnd returns the index of the ']' closing the bracket expression opened at s[i], or -1.
func classEnd(s string, i int) int {
	j := i + 1
	if j < len(s) && (s[j] == '!' || s[j] == '^') {
		j++
	}
	if j < len(s) && s[j] == ']' {
		j++
	}
	for ; j < len(s); j++ {
		switch {
		case s[j] == ']':
			return j
		case s[j] == '[' && j+1 < len(s) && s[j+1] == ':':
			if k := strings.Index(s[j+2:], ":]"); k >= 0 {
				j += k + 3
			}
		}
	}
	return -1
}
