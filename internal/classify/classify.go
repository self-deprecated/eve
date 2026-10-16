// Package classify explains why a job failed: an ordered table of rules seeded from the failures
// mined on Quartz (PLAN.md §9) maps a phase's log to a failure class with a summary, a hint and,
// where a person must act, a needs_human action. Logs no rule explains stay "unknown"; an
// optional Summarizer can describe those, never decide anything.
//
// Logs are expected to be redacted already (internal/redact); nothing here removes secrets.
package classify

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/self-deprecated/eve/internal/api"
)

// Input is what the pipeline knows about a failed phase.
type Input struct {
	Phase    api.State
	Log      []byte // the phase's log, already redacted
	ExitCode int
	// Context holds optional facts that sharpen hints, such as "signing": "api-key".
	Context map[string]string
}

// Verdict is a classification together with the person's action it calls for, if any.
type Verdict struct {
	Failure *api.Failure
	Human   *api.Human // nil unless the job should end needs_human
}

// Limits for summaries and hints, which go into the 4 KB result document (§5.3).
const (
	maxSummary = 300
	maxHint    = 400
	// maxCandidates bounds how many lines one rule inspects in a pathological log.
	maxCandidates = 20000
)

// humanURLs are where a person goes for each action, when there is one place.
var humanURLs = map[string]string{
	api.HumanAcceptAgreement:  "https://appstoreconnect.apple.com/business",
	api.HumanCreateAppRecord:  "https://appstoreconnect.apple.com/apps",
	api.HumanExportCompliance: "https://appstoreconnect.apple.com/apps",
}

// Classify returns the failure for in: the first rule, in table order, that matches the log,
// else the first rule matching the exit code, else ClassUnknown. Excerpt is filled; Log is left
// for the caller.
func Classify(in Input) *api.Failure {
	return Diagnose(in).Failure
}

// Diagnose is Classify plus the needs_human action of the matching rule.
func Diagnose(in Input) Verdict {
	for i := range rules {
		r := &rules[i]
		if len(r.keys) == 0 || !r.inPhase(in.Phase) {
			continue
		}
		if line, groups, ok := r.find(in.Log); ok {
			return r.verdict(in, line, groups)
		}
	}
	if in.ExitCode != 0 {
		for i := range rules {
			r := &rules[i]
			if slices.Contains(r.exit, in.ExitCode) && r.inPhase(in.Phase) {
				return r.verdict(in, "", nil)
			}
		}
	}
	return Verdict{Failure: unknown(in)}
}

// IsDuplicateUpload reports whether log shows Apple refusing an upload because the build number
// already exists (ITMS-90189). The pipeline treats that as "already uploaded" and reconciles
// with App Store Connect instead of failing (§5.5).
func IsDuplicateUpload(log []byte) bool {
	for i := range rules {
		if r := &rules[i]; r.class == ClassUploadDuplicate {
			if _, _, ok := r.find(log); ok {
				return true
			}
		}
	}
	return false
}

// ForClass returns the table's summary and hint for class, for failures eve detects itself
// (build.number_mismatch, asc.processing_deadline, …), or nil if the class isn't in the table.
func ForClass(class string) *api.Failure {
	for i := range rules {
		if r := &rules[i]; r.class == class {
			return &api.Failure{Class: class, Summary: r.summary, Hint: r.hint, By: "rules"}
		}
	}
	return nil
}

// Classes lists every class the table knows, in table order, without duplicates.
func Classes() []string {
	var out []string
	for _, r := range rules {
		if !slices.Contains(out, r.class) {
			out = append(out, r.class)
		}
	}
	return out
}

func (r *rule) inPhase(p api.State) bool {
	return len(r.phases) == 0 || slices.Contains(r.phases, p)
}

// find returns the first line in log order that contains one of r's keys and matches one of
// its patterns, cleaned for display, with the matching pattern's named groups.
func (r *rule) find(log []byte) (string, map[string]string, bool) {
	var starts []int
	for _, key := range r.keys {
		k := []byte(key)
		for pos := 0; pos < len(log) && len(starts) < maxCandidates; {
			i := bytes.Index(log[pos:], k)
			if i < 0 {
				break
			}
			at := pos + i
			start := bytes.LastIndexByte(log[:at], '\n') + 1
			starts = append(starts, start)
			pos = lineEnd(log, at) + 1
		}
	}
	slices.Sort(starts)
	starts = slices.Compact(starts)
	for _, start := range starts {
		line := cleanLine(log[start:lineEnd(log, start)])
		if len(r.match) == 0 {
			return line, nil, true
		}
		for _, p := range r.match {
			if m := p.FindStringSubmatch(line); m != nil {
				return line, groups(p, m), true
			}
		}
	}
	return "", nil, false
}

func lineEnd(log []byte, from int) int {
	if i := bytes.IndexByte(log[from:], '\n'); i >= 0 {
		return from + i
	}
	return len(log)
}

func groups(p *regexp.Regexp, m []string) map[string]string {
	g := map[string]string{}
	for i, name := range p.SubexpNames() {
		if name != "" && m[i] != "" {
			g[name] = m[i]
		}
	}
	return g
}

func (r *rule) verdict(in Input, line string, g map[string]string) Verdict {
	summary, hint := r.summary, r.hint
	if r.detail != nil {
		s, h := r.detail(g, in)
		if s != "" {
			summary = s
		}
		if h != "" {
			hint = h
		}
	}
	f := &api.Failure{
		Class:   r.class,
		Summary: truncate(summary, maxSummary),
		Hint:    truncate(hint, maxHint),
		Excerpt: withLine(Excerpt(in.Log, MaxExcerptLines), line),
		By:      "rules",
	}
	v := Verdict{Failure: f}
	if r.human != "" {
		v.Human = &api.Human{Action: r.human, Detail: f.Hint, URL: humanURLs[r.human]}
	}
	return v
}

func unknown(in Input) *api.Failure {
	phase := string(in.Phase)
	if phase == "" {
		phase = "job"
	}
	exit := ""
	if in.ExitCode != 0 {
		exit = fmt.Sprintf(" (exit %d)", in.ExitCode)
	}
	return &api.Failure{
		Class:   ClassUnknown,
		Summary: fmt.Sprintf("The %s phase failed%s and no rule recognized the log.", phase, exit),
		Hint:    "Read the excerpt; the full log has the rest.",
		Excerpt: Excerpt(in.Log, MaxExcerptLines),
		By:      "rules",
	}
}

// withLine makes sure the matched line leads the excerpt, so the evidence for the class is
// always shown, keeping the excerpt within its limits.
func withLine(ex []string, line string) []string {
	if line == "" {
		return ex
	}
	line = truncate(line, MaxExcerptLineLen)
	if slices.Contains(ex, line) {
		return ex
	}
	ex = append([]string{line}, ex...)
	for len(ex) > MaxExcerptLines || (len(ex) > 1 && size(ex) > MaxExcerptBytes) {
		ex = ex[:len(ex)-1]
	}
	return ex
}

func size(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l)
	}
	return n
}

var (
	ansiRE  = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	stampRE = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?:\.\d+)?(?: [+-]\d{4})? +(?:\S+\[\d+:\d+\] )?(?:\[MT\] )?`)
)

// cleanLine makes a log line fit for matching and display: valid UTF-8, no colour codes, no
// xcodebuild or IDEDistribution timestamp prefix, no surrounding whitespace.
func cleanLine(b []byte) string {
	s := strings.ToValidUTF8(string(b), "\uFFFD")
	if strings.IndexByte(s, 0x1b) >= 0 {
		s = ansiRE.ReplaceAllString(s, "")
	}
	s = strings.TrimSpace(s)
	if len(s) > 20 && s[0] >= '0' && s[0] <= '9' {
		s = strings.TrimSpace(stampRE.ReplaceAllString(s, ""))
	}
	return s
}

// truncate shortens s to at most n runes, ending with "…" when it cut.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i, count := 0, 0
	for i = range s {
		if count == n-1 {
			break
		}
		count++
	}
	return s[:i] + "…"
}
