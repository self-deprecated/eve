package sizeguard

import (
	"regexp"
	"strings"
)

// pattern is one gitignore-style path pattern, as used by allowPatterns in server.yml and by the
// built-in rules: a trailing / matches a directory (and so everything beneath it), a / elsewhere
// anchors the pattern to the bundle root, otherwise it matches a name at any depth. *, ? and
// [...] stay within one path segment; ** spans segments. There is no negation.
type pattern struct {
	re       *regexp.Regexp
	dirOnly  bool
	anchored bool // false: re matches a single path segment, the last one
}

func compilePattern(p string) (pattern, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return pattern{}, false
	}
	var pt pattern
	if strings.HasSuffix(p, "/") {
		pt.dirOnly = true
		p = strings.TrimRight(p, "/")
	}
	pt.anchored = strings.Contains(p, "/")
	p = strings.TrimPrefix(p, "/")
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case strings.HasPrefix(p[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := p[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return pattern{}, false
	}
	pt.re = re
	return pt, true
}

// patterns is an ordered set; a path matches when any pattern does.
type patterns []pattern

func compilePatterns(ps []string) patterns {
	var out patterns
	for _, p := range ps {
		if pt, ok := compilePattern(p); ok {
			out = append(out, pt)
		}
	}
	return out
}

// match reports whether the file at the slash-separated path p, or one of its parent
// directories, matches. It returns the matching path: p itself or the matching directory.
func (ps patterns) match(p string) (string, bool) {
	if len(ps) == 0 {
		return "", false
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '/' && ps.matchOne(p[:i], true) {
			return p[:i], true
		}
	}
	if ps.matchOne(p, false) {
		return p, true
	}
	return "", false
}

func (ps patterns) matchOne(p string, isDir bool) bool {
	name := p[strings.LastIndexByte(p, '/')+1:]
	for _, pt := range ps {
		if isDir || !pt.dirOnly {
			if pt.anchored && pt.re.MatchString(p) || !pt.anchored && pt.re.MatchString(name) {
				return true
			}
		}
	}
	return false
}
