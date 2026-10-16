package sizeguard

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
)

// Rule 1: credentials, which fail the job outright.
var (
	secretPatterns = compilePatterns([]string{
		"*.p8", "*.p12", "*.mobileprovision", ".env*", "*.pem", "*.key", "AuthKey_*",
	})
	// Xcode embeds the signing profile under this name in every bundle; it is not a secret.
	embeddedProfile = "embedded.mobileprovision"
)

// Rule 2: repository content. Each rule's findings are grouped under its label in the report.
type repoRule struct {
	pats patterns
	// byExt groups matches as "412 .swift files" rather than listing each path.
	byExt bool
}

var repoRules = []repoRule{
	{pats: compilePatterns([]string{
		".git/", "node_modules/", "*.xcodeproj/", "*.xcworkspace/", "DerivedData/",
		"/docs/", "/scripts/", "/build/",
		"project.yml", "Package.swift", "Package.resolved", ".DS_Store",
		"/README*", "/*.md",
	})},
	{pats: compilePatterns([]string{"*.swift", "*.m", "*.mm"}), byExt: true},
}

// containerDirs hold extensions and frameworks the project declares; new entries inside them are
// not "new top-level entries" (rule 3).
var containerDirs = map[string]bool{"PlugIns/": true, "Frameworks/": true}

// BundleVerdict is the outcome of CheckBundle.
type BundleVerdict struct {
	// Failed means secret material was found: the job fails and cannot be confirmed (rule 1).
	Failed bool
	// Tripped means rules 2–4 require the agent to confirm before delivery.
	Tripped bool
	// Secrets lists the credential paths found, for the failure message.
	Secrets []string
	// Report is the compact summary for the job result. Its Gate is failed,
	// awaiting_confirmation, baseline_created or passed; device and check builds replace it with
	// report_only. Details and Confirmation are left for the caller.
	Report api.SizeReport
}

// CheckBundle applies the bundle rules of PLAN.md §6.7.2 to cur, comparing it with the app's
// baseline when there is one. expectChange (--expect-size-change) waives growth and the ceiling,
// never repository content or secrets. th.AllowPatterns exempts matching paths from repository
// content, new top-level entries and new large files; allowed bytes still count toward totals.
func CheckBundle(cur Manifest, base *Manifest, th config.BundleThresholds, expectChange bool) BundleVerdict {
	th = config.DefaultSizeGuard().Merge(&config.SizeGuard{Bundle: th}).Bundle
	allow := compilePatterns(th.AllowPatterns)
	c := bundleCheck{cur: cur, base: base, th: th, allow: allow}

	var v BundleVerdict
	v.Secrets = c.secrets()
	repo := c.repoContent()
	growth := c.growth()
	ceiling := c.ceiling()

	var reasons []string
	if len(v.Secrets) > 0 {
		v.Failed = true
		reasons = append(reasons, "secret material: "+list(v.Secrets, 3))
	}
	if repo.any() {
		v.Tripped = true
		reasons = append(reasons, "repository content: "+repo.String())
	}
	waivable := append(growth.reasons(), ceiling...)
	if expectChange {
		for _, r := range waivable {
			reasons = append(reasons, "expected: "+r)
		}
	} else if len(waivable) > 0 {
		v.Tripped = true
		reasons = append(reasons, waivable...)
	}

	v.Report = c.report(v, reasons, repo, growth)
	return v
}

type bundleCheck struct {
	cur   Manifest
	base  *Manifest
	th    config.BundleThresholds
	allow patterns
}

func (c bundleCheck) allowed(p string) bool {
	_, ok := c.allow.match(p)
	return ok
}

// secrets applies rule 1. Allow patterns do not apply.
func (c bundleCheck) secrets() []string {
	var found []string
	for _, e := range c.cur.Entries {
		if path.Base(e.Path) == embeddedProfile {
			continue
		}
		if m, ok := secretPatterns.match(e.Path); ok {
			if m != e.Path {
				m += "/"
			}
			if len(found) == 0 || found[len(found)-1] != m {
				found = append(found, m)
			}
		}
	}
	return found
}

// repoFindings groups rule 2 matches: a directory or file name, or "N .swift files".
type repoFindings struct {
	order []string
	bytes map[string]int64
	count map[string]int
}

func (r *repoFindings) add(key string, size int64) {
	if r.bytes == nil {
		r.bytes, r.count = map[string]int64{}, map[string]int{}
	}
	if _, ok := r.bytes[key]; !ok {
		r.order = append(r.order, key)
	}
	r.bytes[key] += size
	r.count[key]++
}

func (r repoFindings) any() bool { return len(r.order) > 0 }

// keys returns the findings, largest first.
func (r repoFindings) keys() []string {
	ks := append([]string(nil), r.order...)
	sort.SliceStable(ks, func(i, j int) bool { return r.bytes[ks[i]] > r.bytes[ks[j]] })
	return ks
}

func (r repoFindings) String() string {
	var items []string
	for _, k := range r.keys() {
		if ext, ok := strings.CutPrefix(k, "*"); ok {
			n := r.count[k]
			noun := "files"
			if n == 1 {
				noun = "file"
			}
			items = append(items, fmt.Sprintf("%s %s %s", formatCount(n), ext, noun))
			continue
		}
		items = append(items, k)
	}
	return list(items, 5)
}

// onlyFinderMetadata reports whether the only repository content is .DS_Store files.
func (r repoFindings) onlyFinderMetadata() bool {
	for _, k := range r.order {
		if path.Base(strings.TrimSuffix(k, "/")) != ".DS_Store" {
			return false
		}
	}
	return r.any()
}

// repoContent applies rule 2.
func (c bundleCheck) repoContent() repoFindings {
	var f repoFindings
	for _, e := range c.cur.Entries {
		if c.allowed(e.Path) {
			continue
		}
		for _, rule := range repoRules {
			m, ok := rule.pats.match(e.Path)
			if !ok {
				continue
			}
			switch {
			case rule.byExt:
				f.add("*"+path.Ext(e.Path), e.Size)
			case m != e.Path:
				f.add(m+"/", e.Size)
			default:
				f.add(e.Path, e.Size)
			}
			break
		}
	}
	return f
}

// growthFindings holds rule 3 results.
type growthFindings struct {
	total    string   // reason, or ""
	files    string   // reason, or ""
	newTop   []string // new bundle-root entries
	newLarge []Entry  // new files over the threshold, largest first
}

func (g growthFindings) reasons() []string {
	var rs []string
	for _, r := range []string{g.total, g.files} {
		if r != "" {
			rs = append(rs, r)
		}
	}
	if len(g.newTop) > 0 {
		var items []string
		for _, p := range g.newTop {
			items = append(items, shorten(p, 40))
		}
		rs = append(rs, "new in bundle root: "+list(items, 3))
	}
	if len(g.newLarge) > 0 {
		var items []string
		for _, e := range g.newLarge {
			items = append(items, fmt.Sprintf("%s (%s)", shorten(e.Path, 48), FormatBytes(e.Size)))
		}
		rs = append(rs, "new large file: "+list(items, 2))
	}
	return rs
}

// growth applies rule 3; it needs a baseline.
func (c bundleCheck) growth() growthFindings {
	var g growthFindings
	if c.base == nil {
		return g
	}
	base, th := c.base, c.th
	if d := c.cur.Total - base.Total; d > max(int64(float64(base.Total)*float64(th.Growth)/100), int64(th.GrowthMin)) {
		g.total = fmt.Sprintf("growth %s (%s; limit +%g%% or +%s)",
			percent(c.cur.Total, base.Total), formatDelta(d), float64(th.Growth), FormatBytes(int64(th.GrowthMin)))
	}
	if d := c.cur.Files - base.Files; float64(d) > max(float64(base.Files)*float64(th.Files)/100, float64(th.FilesMin)) {
		g.files = fmt.Sprintf("files %s (limit +%g%% or +%s)", formatCountDelta(d), float64(th.Files), formatCount(th.FilesMin))
	}

	baseIdx := base.index()
	baseTops := map[string]bool{}
	for _, e := range base.Entries {
		baseTops[topEntry(e.Path)] = true
	}
	seen := map[string]bool{}
	for _, e := range c.cur.Entries {
		if c.allowed(e.Path) {
			continue
		}
		// Only new top-level directories count: iOS copies plain resources (a sound, an image)
		// into the bundle root, and those are covered by the new-large-file rule. A new directory
		// (docs/, scripts/) is the folder-reference mistake this rule exists for.
		if top := topEntry(e.Path); strings.Contains(e.Path, "/") && !containerDirs[top] && !baseTops[top] && !seen[top] {
			seen[top] = true
			g.newTop = append(g.newTop, top)
		}
		if _, existed := baseIdx[e.Path]; !existed && e.Size > int64(th.NewFile) {
			g.newLarge = append(g.newLarge, e)
		}
	}
	sort.SliceStable(g.newLarge, func(i, j int) bool { return g.newLarge[i].Size > g.newLarge[j].Size })
	return g
}

// ceiling applies rule 4.
func (c bundleCheck) ceiling() []string {
	if c.cur.Total > int64(c.th.Ceiling) {
		return []string{fmt.Sprintf("size %s over the %s ceiling", FormatBytes(c.cur.Total), FormatBytes(int64(c.th.Ceiling)))}
	}
	return nil
}
