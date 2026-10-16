package sizeguard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
)

const (
	// MaxReportBytes caps the JSON size of a SizeReport: it travels in every result and wake-up,
	// so the full diff stays behind `eve size`.
	MaxReportBytes = 1500
	largestEntries = 8
	maxReasonLen   = 180
	maxPathLen     = 56
)

func (c bundleCheck) report(v BundleVerdict, reasons []string, repo repoFindings, g growthFindings) api.SizeReport {
	r := api.SizeReport{AppBytes: c.cur.Total, Files: c.cur.Files}
	if c.base != nil {
		r.BaselineBytes, r.BaselineFiles, r.BaselineBuild = c.base.Total, c.base.Files, c.base.Build
		r.DeltaBytes, r.DeltaFiles = c.cur.Total-c.base.Total, c.cur.Files-c.base.Files
	}
	switch {
	case v.Failed:
		r.Gate = api.GateFailed
	case v.Tripped:
		r.Gate = api.GateAwaitingConfirmation
	case c.base == nil:
		r.Gate = api.GateBaselineCreated
	default:
		r.Gate = api.GatePassed
	}
	r.Summary = c.summary(r, v)
	for _, s := range reasons {
		r.Reasons = append(r.Reasons, shorten(s, maxReasonLen))
	}
	r.Largest = c.largest()
	if v.Failed || v.Tripped {
		r.Hint = c.hint(v, repo, g)
	}
	return fitReport(r)
}

func (c bundleCheck) summary(r api.SizeReport, v BundleVerdict) string {
	name := c.cur.label()
	size := fmt.Sprintf("%s %s, %s files", name, FormatBytes(r.AppBytes), formatCount(r.Files))
	switch {
	case v.Failed:
		return fmt.Sprintf("%s contains secret material (%s); delivery refused", name, shorten(list(v.Secrets, 1), 60))
	case c.base == nil && v.Tripped:
		return size + ": first build, confirm or reject before it becomes the baseline"
	case c.base == nil:
		return size + ": first build, recorded as the baseline"
	}
	vs := fmt.Sprintf("%s, %s files vs baseline %s", formatDelta(r.DeltaBytes), formatCountDelta(r.DeltaFiles), FormatBytes(r.BaselineBytes))
	if r.BaselineBuild != "" {
		vs += " (" + r.BaselineBuild + ")"
	}
	if v.Tripped {
		return size + ": " + vs + "; confirm or reject"
	}
	return size + ": " + vs + "; within limits"
}

// unit is the aggregation level for "largest": a bundle-root entry, or one extension or framework
// inside PlugIns/ and Frameworks/.
func unit(p string) string {
	top := topEntry(p)
	if containerDirs[top] {
		rest := p[len(top):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			return top + rest[:i+1]
		}
		return p
	}
	return top
}

type unitSize struct {
	path      string
	cur, base int64
	inCur     bool
	inBase    bool
}

func aggregate(cur Manifest, base *Manifest, key func(string) string) map[string]*unitSize {
	units := map[string]*unitSize{}
	get := func(p string) *unitSize {
		k := key(p)
		u := units[k]
		if u == nil {
			u = &unitSize{path: k}
			units[k] = u
		}
		return u
	}
	for _, e := range cur.Entries {
		u := get(e.Path)
		u.cur += e.Size
		u.inCur = true
	}
	if base != nil {
		for _, e := range base.Entries {
			u := get(e.Path)
			u.base += e.Size
			u.inBase = true
		}
	}
	return units
}

// change describes how a unit differs from the baseline.
func (u *unitSize) change(haveBase bool) string {
	switch {
	case !haveBase:
		return ""
	case !u.inBase:
		return "new"
	case !u.inCur:
		return "removed"
	case u.cur == u.base:
		return "unchanged"
	}
	return formatDelta(u.cur - u.base)
}

// largest returns the units that explain the bundle: biggest change first, then biggest size.
func (c bundleCheck) largest() []api.SizeEntry {
	units := aggregate(c.cur, c.base, unit)
	us := make([]*unitSize, 0, len(units))
	for _, u := range units {
		us = append(us, u)
	}
	abs := func(n int64) int64 { return max(n, -n) }
	sort.Slice(us, func(i, j int) bool {
		a, b := us[i], us[j]
		if da, db := abs(a.cur-a.base), abs(b.cur-b.base); c.base != nil && da != db {
			return da > db
		}
		if a.cur != b.cur {
			return a.cur > b.cur
		}
		return a.path < b.path
	})
	var out []api.SizeEntry
	for _, u := range us[:min(len(us), largestEntries)] {
		size := u.cur
		if !u.inCur {
			size = u.base
		}
		out = append(out, api.SizeEntry{
			Path:   shorten(c.cur.label()+"/"+u.path, maxPathLen),
			Bytes:  size,
			Change: u.change(c.base != nil),
		})
	}
	return out
}

// hint picks the single most useful next step for the agent.
func (c bundleCheck) hint(v BundleVerdict, repo repoFindings, g growthFindings) string {
	switch {
	case v.Failed:
		return "Remove " + shorten(v.Secrets[0], 60) + " from the app target's resources (Copy Bundle Resources, " +
			"project.yml `sources:`/`resources:`); credentials must never ship. This cannot be confirmed."
	case repo.onlyFinderMetadata():
		return "A .DS_Store was copied into the bundle, usually through a folder reference; delete it and add .DS_Store to .gitignore."
	case repo.any():
		return "The app bundle contains repository files. In project.yml, check `sources:` for a path that " +
			"includes the repo root, or a folder reference; exclude docs/ and scripts/."
	case len(g.newLarge) > 0:
		e := g.newLarge[0]
		return fmt.Sprintf("New file %s (%s): if intended, confirm with the reason; otherwise remove it from the target's resources.",
			shorten(e.Path, 60), FormatBytes(e.Size))
	case len(g.newTop) > 0:
		return "New in the bundle root: " + list(g.newTop, 3) + ". Folder references copy whole directories; " +
			"check project.yml `sources:`/`resources:`, or confirm if intended."
	case g.total != "" || g.files != "":
		if fw := c.frameworksDelta(); fw*2 > c.cur.Total-c.base.Total {
			return "Most growth is in Frameworks/ (" + formatDelta(fw) + "): check for a new or duplicated " +
				"dependency, or a framework embedded twice."
		}
		return "The bundle grew; `eve size` shows every change. Confirm with the reason if expected (new assets, " +
			"a new dependency), otherwise check what the target copies."
	case c.cur.Total > int64(c.th.Ceiling):
		return "The bundle is over the ceiling; if it is legitimately this large, raise sizeGuard.bundle.ceiling " +
			"for the app in server.yml."
	}
	return ""
}

func (c bundleCheck) frameworksDelta() int64 {
	var d int64
	for _, e := range c.cur.Entries {
		if strings.HasPrefix(e.Path, "Frameworks/") {
			d += e.Size
		}
	}
	for _, e := range c.base.Entries {
		if strings.HasPrefix(e.Path, "Frameworks/") {
			d -= e.Size
		}
	}
	return d
}

// fitReport trims r until its JSON fits MaxReportBytes, dropping the least important detail first.
func fitReport(r api.SizeReport) api.SizeReport {
	size := func() int {
		b, _ := json.Marshal(r)
		return len(b)
	}
	for size() > MaxReportBytes && len(r.Largest) > 3 {
		r.Largest = r.Largest[:len(r.Largest)-1]
	}
	for size() > MaxReportBytes && len(r.Reasons) > 2 {
		r.Reasons = r.Reasons[:len(r.Reasons)-1]
	}
	for i := range r.Reasons {
		if size() <= MaxReportBytes {
			break
		}
		r.Reasons[i] = shorten(r.Reasons[i], 80)
	}
	if size() > MaxReportBytes {
		r.Hint = shorten(r.Hint, 100)
	}
	for size() > MaxReportBytes && len(r.Largest) > 0 {
		r.Largest = r.Largest[:len(r.Largest)-1]
	}
	return r
}
