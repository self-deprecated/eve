package queue

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/store"
)

func TestPruneSlimsThenDeletesFinishedJobs(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now()
	clock := now
	st.SetClock(func() time.Time { return clock })

	job := func(name string, finishedAgo time.Duration, to api.State, sha string) string {
		clock = now.Add(-finishedAgo)
		j, _, err := st.CreateJob(ctx, store.Job{
			App: "driver", Verb: api.VerbTestflight, IdempotencyKey: name,
			Request: api.JobRequest{App: "driver", Verb: api.VerbTestflight, Source: api.JobSource{SHA256: sha}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if to != api.StateQueued {
			if _, _, err := st.Transition(ctx, j.ID, store.Transition{To: to}); err != nil {
				t.Fatal(err)
			}
		}
		jd := filepath.Join(dir, "jobs", j.ID)
		for _, p := range []string{"src/ios/Driver.swift", "Driver.xcarchive/Info.plist", "logs/archive.log", "artifacts/current/a.png", "bundle-manifest.json.zst"} {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(jd, p)), 0o700)
			_ = os.WriteFile(filepath.Join(jd, p), []byte(name), 0o600)
		}
		clock = now
		return j.ID
	}
	fresh := job("fresh", time.Hour, api.StateSucceeded, "")
	day := job("day", 2*24*time.Hour, api.StateFailed, "")
	month := job("month", 40*24*time.Hour, api.StateSucceeded, "")
	base := job("base", 40*24*time.Hour, api.StateSucceeded, "")
	running := job("running", 40*24*time.Hour, api.StateQueued, strings.Repeat("b", 64))
	if err := st.SetBaseline(ctx, store.Baseline{App: "driver", JobID: base, Path: "x"}); err != nil {
		t.Fatal(err)
	}
	srcs := filepath.Join(dir, "sources")
	_ = os.MkdirAll(srcs, 0o700)
	for _, sha := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)} {
		p := filepath.Join(srcs, sha+".tar.zst")
		_ = os.WriteFile(p, []byte("zst"), 0o600)
		old := now.Add(-10 * 24 * time.Hour)
		if sha[0] == 'c' {
			old = now // recent
		}
		_ = os.Chtimes(p, old, old)
	}

	q := New(st, dir, nil, nil, nil)
	got, err := q.Prune(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slimmed != 1 || got.Deleted != 1 || got.Sources != 1 {
		t.Fatalf("stats %+v", got)
	}
	left := func(id string) []string {
		var out []string
		_ = filepath.WalkDir(filepath.Join(dir, "jobs", id), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(filepath.Join(dir, "jobs", id), p)
				out = append(out, rel)
			}
			return nil
		})
		return out
	}
	all := []string{"Driver.xcarchive/Info.plist", "artifacts/current/a.png", "bundle-manifest.json.zst", "logs/archive.log", "src/ios/Driver.swift"}
	if !slices.Equal(left(fresh), all) || !slices.Equal(left(running), all) {
		t.Fatalf("fresh %v running %v: untouched", left(fresh), left(running))
	}
	if !slices.Equal(left(day), []string{"artifacts/current/a.png", "bundle-manifest.json.zst", "logs/archive.log"}) {
		t.Fatalf("day-old job keeps logs, artifacts and the manifest: %v", left(day))
	}
	if len(left(month)) != 0 || !slices.Equal(left(base), []string{"bundle-manifest.json.zst"}) {
		t.Fatalf("month %v base %v", left(month), left(base))
	}
	for sha, want := range map[byte]bool{'a': false, 'b': true, 'c': true} {
		if _, err := os.Stat(filepath.Join(srcs, strings.Repeat(string(sha), 64)+".tar.zst")); (err == nil) != want {
			t.Fatalf("source %c kept=%v, want %v (b: an unfinished job needs it; c: recent)", sha, err == nil, want)
		}
	}
}
