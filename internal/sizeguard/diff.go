package sizeguard

import (
	"path"
	"sort"

	"github.com/self-deprecated/eve/internal/api"
)

// Diff returns every difference between cur and base for `eve size`, grouped by directory: each
// directory with changes contributes a header entry ("Driver.app/docs/", its size in cur, and its
// net change), followed by its added, removed, grown and shrunk files. Without a baseline every
// file is "new". Unchanged files are omitted.
func Diff(cur Manifest, base *Manifest) []api.SizeEntry {
	empty := Manifest{}
	if base == nil {
		base = &empty
	}
	curIdx, baseIdx := cur.index(), base.index()
	type change struct {
		path  string
		entry api.SizeEntry
	}
	byDir := map[string][]change{}
	dirs := map[string]*unitSize{}
	note := func(p string, curSize, baseSize int64, inCur, inBase bool) {
		d := path.Dir(p)
		u := dirs[d]
		if u == nil {
			u = &unitSize{path: d}
			dirs[d] = u
		}
		u.cur += curSize
		u.base += baseSize
		u.inCur = u.inCur || inCur
		u.inBase = u.inBase || inBase
	}
	add := func(p string, e api.SizeEntry) {
		d := path.Dir(p)
		byDir[d] = append(byDir[d], change{p, e})
	}
	for _, e := range cur.Entries {
		b, ok := baseIdx[e.Path]
		note(e.Path, e.Size, b.Size, true, ok)
		label := cur.label() + "/" + e.Path
		switch {
		case !ok:
			add(e.Path, api.SizeEntry{Path: label, Bytes: e.Size, Change: "new"})
		case b.Size != e.Size || b.Link != e.Link:
			add(e.Path, api.SizeEntry{Path: label, Bytes: e.Size, Change: formatDelta(e.Size - b.Size)})
		}
	}
	for _, b := range base.Entries {
		if _, ok := curIdx[b.Path]; !ok {
			note(b.Path, 0, b.Size, false, true)
			add(b.Path, api.SizeEntry{Path: cur.label() + "/" + b.Path, Bytes: b.Size, Change: "removed"})
		}
	}

	names := make([]string, 0, len(byDir))
	for d := range byDir {
		names = append(names, d)
	}
	sort.Strings(names)
	var out []api.SizeEntry
	for _, d := range names {
		u := dirs[d]
		header := cur.label() + "/"
		if d != "." {
			header += d + "/"
		}
		size := u.cur
		if !u.inCur {
			size = u.base
		}
		out = append(out, api.SizeEntry{Path: header, Bytes: size, Change: u.change(true)})
		changes := byDir[d]
		sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })
		for _, c := range changes {
			out = append(out, c.entry)
		}
	}
	return out
}
