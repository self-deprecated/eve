// Package redact removes secrets and personal identifiers from build logs before they are stored,
// served or quoted in a result document (PLAN.md §5.3, §6.2 step 4).
//
// A Redactor knows the exact secret values of a job (and their base64 and URL-encoded forms) and
// a fixed set of patterns: the user's home directory, email addresses, Apple device UDIDs,
// bearer and JWT-looking tokens and PEM private-key blocks. It works line by line, so a whole log
// in memory (Redact) and a live stream (Writer) are redacted identically.
package redact

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Replacement texts. They are deliberately distinct so a reader can tell what was removed.
const (
	SecretMark = "[secret]"
	EmailMark  = "[email]"
	UDIDMark   = "[udid]"
	TokenMark  = "[token]"
	KeyMark    = "[private key]"
	HomeMark   = "~"
)

// MinSecretLen is the shortest secret value that is redacted. Shorter values would match
// ordinary log text too often to be worth hiding, and are ignored.
const MinSecretLen = 6

// Options configures a Redactor.
type Options struct {
	// Home is the user's home directory ("/Users/jmo"); every path under it is shown under "~".
	// Empty disables home redaction.
	Home string
	// Secrets are exact values that must never appear: API keys, keychain passwords, webhook
	// secrets. Values shorter than MinSecretLen are ignored.
	Secrets []string
}

// Redactor redacts logs. It is immutable and safe for concurrent use; each Writer it creates
// carries its own state.
type Redactor struct {
	home     []byte
	forms    [][]byte // every encoded form of every secret, longest first
	replacer *strings.Replacer
	maxForm  int
}

var (
	// Emails need a domain whose first label starts with a letter, which keeps asset names such
	// as "AppIcon60x60@2x.png" out; isFileName rejects the rest ("logo@Large.png").
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z][A-Za-z0-9\-]*(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	// JWTs start with a base64url-encoded '{"' and have up to three dot-separated parts.
	jwtRE = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}(?:\.[A-Za-z0-9_\-]{4,}){0,2}`)
	// A bearer credential needs at least 16 token characters, so prose such as "Make sure a
	// bearer token was provided" survives.
	bearerRE   = regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/\-]{16,}=*`)
	pemOneLine = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	pemBegin   = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemEnd     = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	pemHeader  = regexp.MustCompile(`^(Proc-Type|DEK-Info|Comment): `)
)

// New builds a Redactor from opts.
func New(opts Options) *Redactor {
	r := &Redactor{home: []byte(strings.TrimRight(opts.Home, "/"))}
	seen := map[string]bool{}
	add := func(s string) {
		if len(s) >= MinSecretLen && !seen[s] {
			seen[s] = true
			r.forms = append(r.forms, []byte(s))
		}
	}
	for _, s := range opts.Secrets {
		if len(s) < MinSecretLen {
			continue
		}
		for _, f := range secretForms(s) {
			add(f)
		}
		// Redaction works on lines, so a multi-line secret is also hidden line by line.
		if strings.ContainsAny(s, "\r\n") {
			for _, part := range strings.FieldsFunc(s, func(c rune) bool { return c == '\n' || c == '\r' }) {
				add(part)
			}
		}
	}
	// strings.Replacer prefers earlier arguments at the same position: longest first, so an
	// encoded form wins over a shorter form it contains.
	slices.SortStableFunc(r.forms, func(a, b []byte) int { return len(b) - len(a) })
	if len(r.forms) > 0 {
		pairs := make([]string, 0, 2*len(r.forms))
		for _, f := range r.forms {
			pairs = append(pairs, string(f), SecretMark)
		}
		r.replacer = strings.NewReplacer(pairs...)
		r.maxForm = len(r.forms[0])
	}
	return r
}

// secretForms returns s and the forms it takes when a tool encodes it: URL escaping and base64.
// For base64 it also returns the part of the encoding that does not depend on the bytes around
// s, at each of the three possible alignments, so a secret inside a larger encoded blob (an
// Authorization header, a JSON payload) is still found.
func secretForms(s string) []string {
	forms := []string{s, url.QueryEscape(s), url.PathEscape(s)}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		forms = append(forms, enc.EncodeToString([]byte(s)))
		raw := enc.WithPadding(base64.NoPadding)
		for k := range 3 {
			padded := append(make([]byte, k), s...)
			full := raw.EncodeToString(padded)
			// Character i covers bits [6i, 6i+6); keep the ones made only of bits of s.
			start := (8*k + 5) / 6
			end := 8 * (k + len(s)) / 6
			if end-start >= MinSecretLen {
				forms = append(forms, full[start:end])
			}
		}
	}
	return forms
}

// Redact returns a redacted copy of b.
func (r *Redactor) Redact(b []byte) []byte {
	var st state
	out := make([]byte, 0, len(b)+len(b)/32)
	return r.appendLines(out, b, &st)
}

// String redacts s.
func (r *Redactor) String(s string) string {
	return string(r.Redact([]byte(s)))
}

// state is what redaction carries from one line to the next.
type state struct {
	inKey bool // inside a multi-line private-key block
}

// blockHints records which redactions can apply anywhere in a block of lines, so the per-line
// work is skipped for the common case: a long xcodebuild log with no secrets in it.
type blockHints struct {
	forms  [][]byte // secret forms present somewhere in the block
	tokens bool
	email  bool
	pem    bool
	home   bool
}

func (r *Redactor) hints(block []byte, st *state) blockHints {
	var h blockHints
	for _, f := range r.forms {
		if bytes.Contains(block, f) {
			h.forms = append(h.forms, f)
		}
	}
	h.tokens = bytes.Contains(block, []byte("eyJ")) || bytes.Contains(block, []byte("earer")) ||
		bytes.Contains(block, []byte("EARER"))
	h.email = bytes.IndexByte(block, '@') >= 0
	h.pem = st.inKey || bytes.Contains(block, []byte("PRIVATE KEY-----"))
	h.home = len(r.home) > 0 && bytes.Contains(block, r.home)
	return h
}

// appendLines appends the redacted form of block to dst. A final segment without a newline is
// treated as a line and stays without one.
func (r *Redactor) appendLines(dst, block []byte, st *state) []byte {
	h := r.hints(block, st)
	var scratch [2][]byte
	for len(block) > 0 {
		line, rest, nl := bytes.Cut(block, []byte{'\n'})
		block = rest
		var keep bool
		dst, keep = r.appendLine(dst, line, st, &h, &scratch)
		if nl && keep {
			dst = append(dst, '\n')
		}
	}
	return dst
}

// appendLine appends the redacted line (without its newline) and reports whether the line's
// newline should follow; lines inside a private-key block disappear entirely.
func (r *Redactor) appendLine(dst, line []byte, st *state, h *blockHints, scratch *[2][]byte) ([]byte, bool) {
	cur := line
	which := 0
	// step replaces cur with f's output when f changed it, alternating scratch buffers.
	step := func(f func(dst, src []byte) ([]byte, bool)) {
		out, changed := f(scratch[which][:0], cur)
		if changed {
			scratch[which] = out
			cur = out
			which ^= 1
		}
	}

	if h.pem {
		if st.inKey {
			if loc := pemEnd.FindIndex(cur); loc != nil {
				st.inKey = false
				cur = cur[loc[1]:]
				if len(bytes.TrimSpace(cur)) == 0 {
					return dst, false
				}
			} else if looksLikeKeyBody(cur) {
				return dst, false
			} else {
				// The block ended without an END line (a truncated key): stop hiding lines.
				st.inKey = false
			}
		}
		step(func(dst, src []byte) ([]byte, bool) { return r.redactPEM(dst, src, st) })
	}
	if len(h.forms) > 0 {
		for _, f := range h.forms {
			if bytes.Contains(cur, f) {
				out := []byte(r.replacer.Replace(string(cur)))
				scratch[which] = out
				cur = out
				which ^= 1
				break
			}
		}
	}
	if h.tokens {
		step(redactTokens)
	}
	if h.email && bytes.IndexByte(cur, '@') >= 0 {
		step(redactEmails)
	}
	step(redactIDs)
	if h.home {
		step(r.redactHome)
	}
	return append(dst, cur...), true
}

// looksLikeKeyBody reports whether line could be part of a PEM body: base64, or an RFC 1421
// header such as "Proc-Type: 4,ENCRYPTED".
func looksLikeKeyBody(line []byte) bool {
	t := bytes.TrimSpace(line)
	if len(t) == 0 || pemHeader.Match(t) {
		return true
	}
	isBase64 := func(c byte) bool { return isWord(c) && c != '_' || c == '+' || c == '/' || c == '=' }
	for _, c := range t {
		if !isBase64(c) {
			return false
		}
	}
	return true
}

func (r *Redactor) redactPEM(dst, src []byte, st *state) ([]byte, bool) {
	if !bytes.Contains(src, []byte("PRIVATE KEY-----")) {
		return dst, false
	}
	src = pemOneLine.ReplaceAll(src, []byte(KeyMark))
	if loc := pemBegin.FindIndex(src); loc != nil {
		// A block starts here and continues on the following lines.
		st.inKey = true
		src = append(src[:loc[0]:loc[0]], KeyMark...)
	}
	return append(dst, src...), true
}

func redactTokens(dst, src []byte) ([]byte, bool) {
	changed := false
	if bytes.Contains(src, []byte("eyJ")) && jwtRE.Match(src) {
		src = jwtRE.ReplaceAll(src, []byte(TokenMark))
		changed = true
	}
	if bearerRE.Match(src) {
		src = bearerRE.ReplaceAll(src, []byte("${1}"+TokenMark))
		changed = true
	}
	if !changed {
		return dst, false
	}
	return append(dst, src...), true
}

// fileExts are "top-level domains" that are really file extensions of names containing '@'.
var fileExts = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true, "pdf": true, "svg": true, "heic": true,
	"json": true, "plist": true, "strings": true, "swift": true, "car": true, "nib": true,
	"storyboardc": true, "xcassets": true, "framework": true, "app": true, "appex": true,
	"dylib": true, "txt": true, "log": true, "xib": true, "storyboard": true, "imageset": true,
}

func redactEmails(dst, src []byte) ([]byte, bool) {
	changed := false
	out := emailRE.ReplaceAllFunc(src, func(m []byte) []byte {
		tld := m[bytes.LastIndexByte(m, '.')+1:]
		if fileExts[strings.ToLower(string(tld))] {
			return m
		}
		changed = true
		return []byte(EmailMark)
	})
	if !changed {
		return dst, false
	}
	return append(dst, out...), true
}

func isWord(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// idAt reports the length of a device identifier starting at word boundary i: the 25-character
// form "00008150-0011AABB22CC33DD", or a 40-hex-digit UDID (devices before 2018) that the line
// labels as an id ("UDID …", "id:…", "identifier = …"). Unlabelled 40-hex runs are git object
// names and certificate SHA-1s, and stay readable. The pipeline also redacts the exact
// identifiers of the phone it installs on.
func idAt(s []byte, i int) int {
	j := i
	for j < len(s) && isHex(s[j]) {
		j++
	}
	run := j - i
	if run == 40 && (j == len(s) || !isWord(s[j])) && labelledID(s[:i]) {
		return 40
	}
	if run == 8 && bytes.HasPrefix(s[i:], []byte("0000")) && j < len(s) && s[j] == '-' {
		k := j + 1
		for k < len(s) && isHex(s[k]) {
			k++
		}
		if k-j-1 == 16 && (k == len(s) || !isWord(s[k])) {
			return k - i
		}
	}
	return 0
}

// labelledID reports whether before ends with an id label: "UDID", "id:", "deviceId=",
// "Identifier = ", ignoring case, spaces and one ':' or '='.
func labelledID(before []byte) bool {
	b := bytes.TrimRight(before, " \t")
	b = bytes.TrimRight(bytes.TrimSuffix(bytes.TrimSuffix(b, []byte(":")), []byte("=")), " \t")
	k := len(b)
	for k > 0 && (isWord(b[k-1]) || b[k-1] == '-') {
		k--
	}
	word := strings.ToLower(string(b[k:]))
	return strings.HasSuffix(word, "id") || strings.HasSuffix(word, "identifier")
}

// redactIDs replaces device identifiers. It is a single scan without regular expressions,
// because it has no cheap prefilter and runs on every line.
func redactIDs(dst, src []byte) ([]byte, bool) {
	changed := false
	last := 0
	for i := 0; i < len(src); {
		if !isWord(src[i]) {
			i++
			continue
		}
		if n := idAt(src, i); n > 0 {
			dst = append(dst, src[last:i]...)
			dst = append(dst, UDIDMark...)
			i += n
			last = i
			changed = true
			continue
		}
		for i < len(src) && isWord(src[i]) {
			i++
		}
	}
	if !changed {
		return dst, false
	}
	return append(dst, src[last:]...), true
}

// isPathChar reports whether c can continue a path component, so "/Users/jmo" does not match
// inside "/Users/jmoore".
func isPathChar(c byte) bool {
	return isWord(c) || c == '.' || c == '-'
}

func (r *Redactor) redactHome(dst, src []byte) ([]byte, bool) {
	changed := false
	last := 0
	for i := 0; ; {
		j := bytes.Index(src[i:], r.home)
		if j < 0 {
			break
		}
		at := i + j
		end := at + len(r.home)
		if end == len(src) || !isPathChar(src[end]) {
			dst = append(dst, src[last:at]...)
			dst = append(dst, HomeMark...)
			last = end
			changed = true
		}
		i = end
	}
	if !changed {
		return dst, false
	}
	return append(dst, src[last:]...), true
}
