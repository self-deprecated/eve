package queue

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Retention (docs/design.md §4.3). A finished job's workspace (repository copy, unpacked upload,
// archive, build products) is only useful while it runs, so it shrinks to what the API still
// serves; the rest goes later. Unfinished jobs are never touched.
const (
	SlimAfter        = 24 * time.Hour      // then keep only logs, artifacts and the size manifest
	DeleteAfter      = 30 * 24 * time.Hour // then delete the job directory (a baseline keeps its manifest)
	SourceRetention  = 7 * 24 * time.Hour  // uploads no unfinished job needs
	manifestFile     = "bundle-manifest.json.zst"
	pruneEvery       = time.Hour
	sourceUploadExt  = ".tar.zst"
	jobsDir, srcsDir = "jobs", "sources"
)

// keptWhenSlim are the entries of a job directory that outlive its workspace.
var keptWhenSlim = map[string]bool{"logs": true, "artifacts": true, manifestFile: true}

// PruneStats says what one pass removed.
type PruneStats struct {
	Slimmed, Deleted, Sources int
	Bytes                     int64
}

// Prune applies the retention rules as of now.
func (q *Queue) Prune(ctx context.Context, now time.Time) (PruneStats, error) {
	var st PruneStats
	baselines, err := q.Store.BaselineJobs(ctx)
	if err != nil {
		return st, err
	}
	old, err := q.Store.FinishedBefore(ctx, now.Add(-SlimAfter))
	if err != nil {
		return st, err
	}
	for _, j := range old {
		dir := filepath.Join(q.DataDir, jobsDir, j.ID)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // already gone
		}
		remove := func(name string) {
			p := filepath.Join(dir, name)
			st.Bytes += size(p)
			_ = os.RemoveAll(p)
		}
		switch {
		case now.Sub(j.FinishedAt) < DeleteAfter:
			slimmed := false
			for _, e := range entries {
				if !keptWhenSlim[e.Name()] {
					remove(e.Name())
					slimmed = true
				}
			}
			if slimmed {
				st.Slimmed++
			}
		case baselines[j.ID]:
			for _, e := range entries {
				if e.Name() != manifestFile {
					remove(e.Name())
				}
			}
		default:
			st.Bytes += size(dir)
			_ = os.RemoveAll(dir)
			st.Deleted++
		}
	}
	unfinished, err := q.Store.Unfinished(ctx)
	if err != nil {
		return st, err
	}
	inUse := map[string]bool{}
	for _, j := range unfinished {
		inUse[j.Request.Source.SHA256] = true
	}
	uploads, _ := os.ReadDir(filepath.Join(q.DataDir, srcsDir))
	for _, e := range uploads {
		sha, ok := strings.CutSuffix(e.Name(), sourceUploadExt)
		fi, err := e.Info()
		if !ok || err != nil || inUse[sha] || now.Sub(fi.ModTime()) < SourceRetention {
			continue
		}
		st.Bytes += fi.Size()
		if os.Remove(filepath.Join(q.DataDir, srcsDir, e.Name())) == nil {
			st.Sources++
		}
	}
	return st, nil
}

// prunePeriodically runs Prune at most once per pruneEvery.
func (q *Queue) prunePeriodically(ctx context.Context) {
	now := time.Now()
	if now.Sub(q.lastPrune) < pruneEvery {
		return
	}
	q.lastPrune = now
	st, err := q.Prune(ctx, now)
	switch {
	case err != nil:
		q.Log.Warn("prune", "err", err)
	case st.Slimmed+st.Deleted+st.Sources > 0:
		q.Log.Info("pruned", "slimmed", st.Slimmed, "deleted", st.Deleted, "sources", st.Sources, "mb", st.Bytes>>20)
	}
}

func size(path string) int64 {
	var n int64
	_ = filepath.Walk(path, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}
