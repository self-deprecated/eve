package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/store"
)

// screenshots acts like a screenshot script: it writes into $EVE_ARTIFACTS, then exits with exit.
func screenshots(exit int, delay time.Duration) func(c run.Cmd) (string, int, error) {
	return func(c run.Cmd) (string, int, error) {
		if c.Name == "/usr/bin/xcode-select" {
			return "/Applications/Xcode.app/Contents/Developer\n", 0, nil
		}
		var dir string
		for _, kv := range c.Env {
			if v, ok := strings.CutPrefix(kv, "EVE_ARTIFACTS="); ok {
				dir = v
			}
		}
		_ = os.MkdirAll(filepath.Join(dir, "current"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "current", "01-home.png"), []byte("png"), 0o644)
		_ = os.MkdirAll(filepath.Join(dir, "mockups"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "report.txt"), []byte("2 screens"), 0o644)
		_ = os.Symlink("/etc", filepath.Join(dir, "link"))
		time.Sleep(delay)
		return "Test Suite 'ScreenshotTests' passed\n", exit, nil
	}
}

func newExecHarness(t *testing.T, allow bool, handler func(run.Cmd) (string, int, error)) (*Deps, *queue.Queue, *store.Store, *run.Fake, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := filepath.Join(dir, "screengate")
	_ = os.MkdirAll(filepath.Join(repo, "ios", "ScreenGate.xcodeproj"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "eve.yml"), []byte("app: screengate\nproject: {dir: ios, xcodeproj: ScreenGate.xcodeproj, scheme: ScreenGate}\n"), 0o644)
	cfg := &config.Server{DataDir: dir, Apps: map[string]config.App{"screengate": {BundleID: "ai.selfdeprecated.screengate", AllowExec: allow}}}
	f := &run.Fake{Handler: handler}
	d := &Deps{Config: cfg, Store: st, Runner: f, Sources: fakeSources{}, Classifier: fakeClassifier{}}
	q := queue.New(st, dir, map[api.Verb]queue.Pipeline{api.VerbExec: NewExec(d)}, nil, nil)
	return d, q, st, f, repo
}

func runExec(t *testing.T, q *queue.Queue, st *store.Store, repo string, argv ...string) store.Job {
	t.Helper()
	j, _, err := st.CreateJob(context.Background(), store.Job{App: "screengate", Verb: api.VerbExec, Request: api.JobRequest{
		App: "screengate", Verb: api.VerbExec, Options: api.JobOptions{Command: argv}, Source: api.JobSource{Path: repo},
	}})
	if err != nil {
		t.Fatal(err)
	}
	q.Exec(context.Background(), j)
	got, _ := st.Get(context.Background(), j.ID)
	return got
}

func TestExecRunsTheRepoCommandAndListsItsArtifacts(t *testing.T) {
	_, q, st, f, repo := newExecHarness(t, true, screenshots(0, 0))
	got := runExec(t, q, st, repo, "scripts/screenshots.sh", "current")
	if got.State != api.StateSucceeded {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
	if !slices.Equal(got.Result.Artifacts, []string{"current/", "mockups/", "report.txt"}) {
		t.Fatalf("artifacts %v", got.Result.Artifacts)
	}
	if n := got.Result.Notes; len(n) != 1 || !strings.Contains(n[0].Detail, "link") {
		t.Fatalf("notes %+v: the symlink is reported, not served", n)
	}
	c, _ := f.Find("screenshots.sh")
	src := filepath.Join(filepath.Dir(c.Name), "..")
	if !strings.HasSuffix(c.Name, filepath.Join("src", "scripts", "screenshots.sh")) || filepath.Clean(src) != c.Dir ||
		!slices.Equal(c.Args, []string{"current"}) {
		t.Fatalf("ran %s %v in %s: the job's copy of the repo, from its root", c.Name, c.Args, c.Dir)
	}
	if !slices.ContainsFunc(c.Env, func(kv string) bool { return strings.HasPrefix(kv, "EVE_PROJECT_DIR=") && strings.HasSuffix(kv, "ios") }) {
		t.Fatalf("env %v", c.Env)
	}
}

func TestExecFailureKeepsItsArtifactsAndTheLogTail(t *testing.T) {
	_, q, st, _, repo := newExecHarness(t, true, screenshots(3, 0))
	got := runExec(t, q, st, repo, "scripts/screenshots.sh")
	f := got.Result.Failure
	if got.State != api.StateFailed || f == nil || f.Class != "exec.failed" || f.Summary != "scripts/screenshots.sh exited 3" {
		t.Fatalf("%s %+v", got.State, f)
	}
	if !slices.Contains(f.Excerpt, "Test Suite 'ScreenshotTests' passed") || !strings.Contains(f.Hint, "eve artifacts") || len(got.Result.Artifacts) != 3 {
		t.Fatalf("%+v artifacts %v", f, got.Result.Artifacts)
	}
}

func TestExecTimesOut(t *testing.T) {
	d, q, st, _, repo := newExecHarness(t, true, screenshots(0, 20*time.Millisecond))
	d.ExecTimeout = time.Millisecond
	got := runExec(t, q, st, repo, "scripts/screenshots.sh")
	if got.State != api.StateFailed || got.Result.Failure == nil || got.Result.Failure.Class != "exec.timeout" {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
}

func TestExecNeedsAllowExec(t *testing.T) {
	_, q, st, f, repo := newExecHarness(t, false, screenshots(0, 0))
	got := runExec(t, q, st, repo, "scripts/screenshots.sh")
	if got.State != api.StateFailed || got.Result.Failure == nil || got.Result.Failure.Class != "config" {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
	if _, err := f.Find("screenshots.sh"); err == nil {
		t.Fatal("the command ran without allowExec")
	}
}
