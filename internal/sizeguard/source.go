// Package sizeguard keeps whole repositories out of uploads and app bundles (PLAN.md §6.7): the
// source check runs on the client before upload, the bundle check on the server after
// archiving, against the app's baseline manifest. It depends only on api and config, so the
// client and server share it without pulling in packing or storage.
package sizeguard

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
)

// SourceFile is one file of a source listing, as the size guard sees it.
type SourceFile struct {
	Path string // slash-separated, relative to the repository root
	Size int64
}

// SourceInput describes the tree the client is about to upload.
type SourceInput struct {
	// Stats summarizes Files. TopDir is keyed by first path segment ("ios", "README.md"); when
	// TopDir is nil the stats are computed from Files.
	Stats api.SourceStats
	// Files are the files that will be uploaded.
	Files []SourceFile
	// Excluded are files git would have uploaded that eve's always-excluded list dropped (build
	// output, node_modules, credentials). Their never-source directories still trip the check,
	// because they mean the .gitignore is incomplete.
	Excluded []SourceFile
}

// neverSource are directory names that are almost never source (PLAN.md §6.7.1). .git is left
// out: it is never listed and always excluded.
var neverSource = []string{"node_modules", "DerivedData", ".build", "build", "Pods"}

// SourceVerdict is the outcome of CheckSource.
type SourceVerdict struct {
	// Tripped means the client must refuse to upload (exit 76) unless --accept-source-size.
	Tripped bool
	Reasons []string
	// Largest are the five largest top-level entries, with their change against the last
	// accepted source ("new", "+1.2 MB"; empty without one).
	Largest []api.SizeEntry
	// Suggestions are ignore lines (for .gitignore, or .eveignore for eve only) that would
	// exclude each new or never-source directory and each oversized file.
	Suggestions []string
	Stats       api.SourceStats
	Last        *api.SourceStats
}

// SourceStatsOf summarizes files the way SourceInput.Stats expects.
func SourceStatsOf(files []SourceFile) api.SourceStats {
	st := api.SourceStats{TopDir: map[string]int64{}}
	for _, f := range files {
		st.Bytes += f.Size
		st.Files++
		top, _, _ := strings.Cut(f.Path, "/")
		st.TopDir[top] += f.Size
	}
	return st
}

// CheckSource applies the source rules of PLAN.md §6.7.1. last is the app's last accepted
// source; without one (the first submit) only the ceiling, never-source directories and large
// files are checked. Zero thresholds take the defaults.
func CheckSource(in SourceInput, last *api.SourceStats, th config.SourceThresholds) SourceVerdict {
	th = config.DefaultSizeGuard().Merge(&config.SizeGuard{Source: th}).Source
	cur := in.Stats
	if cur.TopDir == nil {
		cur = SourceStatsOf(in.Files)
	}
	v := SourceVerdict{Stats: cur, Last: last}
	var suggest []string

	if last != nil {
		if d := cur.Bytes - last.Bytes; d > max(int64(float64(last.Bytes)*float64(th.Growth)/100), int64(th.GrowthMin)) {
			v.Reasons = append(v.Reasons, fmt.Sprintf("size %s vs %s last accepted (%s, %s; limit +%g%% or +%s)",
				FormatBytes(cur.Bytes), FormatBytes(last.Bytes), percent(cur.Bytes, last.Bytes), formatDelta(d),
				float64(th.Growth), FormatBytes(int64(th.GrowthMin))))
		}
		if d := cur.Files - last.Files; last.Files > 0 && float64(d) > float64(last.Files)*float64(th.Files)/100 {
			v.Reasons = append(v.Reasons, fmt.Sprintf("files %s vs %s last accepted (%s; limit +%g%%)",
				formatCount(cur.Files), formatCount(last.Files), percent(int64(cur.Files), int64(last.Files)), float64(th.Files)))
		}
	}
	if cur.Bytes > int64(th.Ceiling) {
		v.Reasons = append(v.Reasons, fmt.Sprintf("size %s over the %s ceiling", FormatBytes(cur.Bytes), FormatBytes(int64(th.Ceiling))))
	}
	for _, d := range neverSourceDirs(in, th.AllowDirs) {
		r := fmt.Sprintf("contains %s/ (%s, %s files)", d.path, FormatBytes(d.bytes), formatCount(d.files))
		if d.excludedOnly {
			r += "; eve leaves it out of the upload, but .gitignore does not exclude it"
		}
		v.Reasons = append(v.Reasons, r)
		suggest = append(suggest, escapeIgnore(path.Base(d.path))+"/")
	}
	var large []SourceFile
	for _, f := range in.Files {
		if f.Size > int64(th.LargeFile) {
			large = append(large, f)
		}
	}
	sort.Slice(large, func(i, j int) bool { return large[i].Size > large[j].Size })
	if len(large) > 0 {
		var items []string
		for _, f := range large {
			items = append(items, fmt.Sprintf("%s (%s)", f.Path, FormatBytes(f.Size)))
		}
		v.Reasons = append(v.Reasons, fmt.Sprintf("files over %s: %s", FormatBytes(int64(th.LargeFile)), list(items, 3)))
	}

	v.Largest = largestTop(cur, last, in.Files)
	newDirs := map[string]bool{}
	if last != nil {
		dirs := topDirs(in.Files)
		for _, e := range v.Largest {
			if top := strings.TrimSuffix(e.Path, "/"); e.Change == "new" && dirs[top] {
				newDirs[top] = true
				suggest = append(suggest, "/"+escapeIgnore(e.Path))
			}
		}
	}
	for _, f := range large {
		if top, _, _ := strings.Cut(f.Path, "/"); !newDirs[top] {
			suggest = append(suggest, "/"+escapeIgnore(f.Path))
		}
	}
	v.Tripped = len(v.Reasons) > 0
	v.Suggestions = dedupe(suggest)
	return v
}

type dirHit struct {
	path         string
	bytes        int64
	files        int
	excludedOnly bool
}

// neverSourceDirs finds the outermost never-source directory of every file, grouped by path.
func neverSourceDirs(in SourceInput, allow []string) []dirHit {
	allowed := map[string]bool{}
	for _, a := range allow {
		allowed[strings.Trim(a, "/")] = true
	}
	never := map[string]bool{}
	for _, n := range neverSource {
		never[n] = true
	}
	hits := map[string]*dirHit{}
	scan := func(files []SourceFile, excluded bool) {
		for _, f := range files {
			segs := strings.Split(f.Path, "/")
			for i, s := range segs[:len(segs)-1] {
				if !never[s] {
					continue
				}
				p := strings.Join(segs[:i+1], "/")
				if allowed[s] || allowed[p] {
					break
				}
				h := hits[p]
				if h == nil {
					h = &dirHit{path: p, excludedOnly: true}
					hits[p] = h
				}
				h.bytes += f.Size
				h.files++
				h.excludedOnly = h.excludedOnly && excluded
				break
			}
		}
	}
	scan(in.Files, false)
	scan(in.Excluded, true)
	out := make([]dirHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].bytes != out[j].bytes {
			return out[i].bytes > out[j].bytes
		}
		return out[i].path < out[j].path
	})
	return out
}

// topDirs reports which top-level entries are directories.
func topDirs(files []SourceFile) map[string]bool {
	dirs := map[string]bool{}
	for _, f := range files {
		if top, _, ok := strings.Cut(f.Path, "/"); ok {
			dirs[top] = true
		}
	}
	return dirs
}

func largestTop(cur api.SourceStats, last *api.SourceStats, files []SourceFile) []api.SizeEntry {
	dirs := topDirs(files)
	keys := make([]string, 0, len(cur.TopDir))
	for k := range cur.TopDir {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if a, b := cur.TopDir[keys[i]], cur.TopDir[keys[j]]; a != b {
			return a > b
		}
		return keys[i] < keys[j]
	})
	var out []api.SizeEntry
	for _, k := range keys[:min(len(keys), 5)] {
		e := api.SizeEntry{Path: k, Bytes: cur.TopDir[k]}
		if dirs[k] {
			e.Path += "/"
		}
		if last != nil {
			if prev, ok := last.TopDir[k]; ok {
				e.Change = formatDelta(e.Bytes - prev)
			} else {
				e.Change = "new"
			}
		}
		out = append(out, e)
	}
	return out
}

// escapeIgnore quotes glob characters so an ignore line matches p literally.
func escapeIgnore(p string) string {
	var b strings.Builder
	for _, c := range p {
		if strings.ContainsRune(`*?[\`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Render formats the verdict for a terminal or an agent: one line when the check passed, the
// reasons, largest entries and suggested ignore lines when it tripped.
func (v SourceVerdict) Render() string {
	var b strings.Builder
	head := fmt.Sprintf("%s, %s files", FormatBytes(v.Stats.Bytes), formatCount(v.Stats.Files))
	if v.Last != nil {
		head += fmt.Sprintf(" (last accepted: %s, %s files)", FormatBytes(v.Last.Bytes), formatCount(v.Last.Files))
	} else {
		head += " (no accepted source yet)"
	}
	if !v.Tripped {
		return "source check passed: " + head + "\n"
	}
	fmt.Fprintf(&b, "source check failed: %s\n", head)
	for _, r := range v.Reasons {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	if len(v.Largest) > 0 {
		b.WriteString("largest top-level entries:\n")
		w := 0
		for _, e := range v.Largest {
			w = max(w, len(e.Path))
		}
		for _, e := range v.Largest {
			fmt.Fprintf(&b, "  %-*s  %9s  %s\n", w, e.Path, FormatBytes(e.Bytes), e.Change)
		}
	}
	if len(v.Suggestions) > 0 {
		b.WriteString("suggested ignore lines (.gitignore, or .eveignore for eve only):\n")
		for _, s := range v.Suggestions {
			fmt.Fprintf(&b, "  %s\n", s)
		}
	}
	b.WriteString(`Fix the ignore rules and resubmit, or resubmit with --accept-source-size "<reason>".` + "\n")
	return b.String()
}
