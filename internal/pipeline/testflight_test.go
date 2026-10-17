package pipeline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/asc"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/store"
)

// --- fakes ---

type fakeASC struct {
	mu         sync.Mutex
	builds     []*asc.Build // successive FindBuild answers; the last one repeats
	recon      asc.Reconciliation
	reconN     int
	latest     string
	added      []string
	whatToTest string
	invite     asc.InviteState
}

func (f *fakeASC) AppByBundleID(ctx context.Context, b string) (asc.App, error) {
	return asc.App{ID: "app-1", BundleID: b}, nil
}

func (f *fakeASC) LatestBuildNumber(ctx context.Context, appID, version string) (string, bool, error) {
	return f.latest, f.latest != "", nil
}

func (f *fakeASC) FindBuild(ctx context.Context, appID, version, number string) (*asc.Build, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.builds) == 0 {
		return nil, nil
	}
	b := f.builds[0]
	if len(f.builds) > 1 {
		f.builds = f.builds[1:]
	}
	return b, nil
}

func (f *fakeASC) ReconcileUntil(ctx context.Context, appID, version, number string, window, interval time.Duration) (asc.Reconciliation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconN++
	return f.recon, nil
}

func (f *fakeASC) BetaGroupByName(ctx context.Context, appID, name string) (asc.BetaGroup, error) {
	return asc.BetaGroup{ID: "g1", Name: name, Internal: true}, nil
}

func (f *fakeASC) AddBuildToGroup(ctx context.Context, groupID, buildID string) error {
	f.added = append(f.added, buildID)
	return nil
}

func (f *fakeASC) SetWhatToTest(ctx context.Context, buildID, text string) (string, error) {
	f.whatToTest = text
	return "en-US", nil
}

func (f *fakeASC) TesterInviteState(ctx context.Context, appID, email string) (asc.TesterInvite, error) {
	s := f.invite
	if s == "" {
		s = asc.InviteAccepted
	}
	return asc.TesterInvite{State: s, AppleState: strings.ToUpper(string(s))}, nil
}

type fakeSources struct{}

func (fakeSources) Extract(sha, dest string) error { return errors.New("no uploads in this test") }
func (fakeSources) Clone(src, dst string) error {
	return exec.Command("/bin/cp", "-R", src, dst).Run()
}

type fakeSize struct{ verdict SizeVerdict }

func (f fakeSize) Measure(appDir, manifestPath string) error {
	return os.WriteFile(manifestPath, []byte("manifest"), 0o600)
}

func (f fakeSize) Check(m, b, bb string, th config.BundleThresholds, expect bool, hint string) (SizeVerdict, error) {
	v := f.verdict
	if v.Report.Summary == "" {
		v.Report = api.SizeReport{Gate: api.GatePassed, Summary: "12.0 MB, 20 files (+0 B)", AppBytes: 12_000_000, Files: 20}
	}
	if expect {
		v.Trip = false
	}
	return v, nil
}

type fakeClassifier struct{}

func (fakeClassifier) Classify(phase api.State, log []byte, exit int, facts map[string]string) (*api.Failure, *api.Human) {
	if strings.Contains(string(log), "Cloud signing permission error") {
		return &api.Failure{Class: "signing.cloud_permission", Summary: "cloud signing", By: "rules"}, nil
	}
	if strings.Contains(string(log), "Your session has expired") {
		return &api.Failure{Class: "xcode.account", Summary: "Xcode account", By: "rules"},
			&api.Human{Action: api.HumanXcodeSignIn, Detail: "sign in to Xcode on Quartz"}
	}
	if strings.Contains(string(log), "could not be, unlocked") {
		return &api.Failure{Class: "device.locked", Summary: "The app is installed but couldn't launch because the phone is locked.", By: "rules"},
			&api.Human{Action: api.HumanUnlockPhone}
	}
	return &api.Failure{Class: "unknown", Summary: "unrecognised failure", By: "rules"}, nil
}

func (fakeClassifier) IsDuplicateUpload(log []byte) bool {
	return strings.Contains(string(log), "ITMS-90189")
}

// xcodeSim answers like Apple's tools.
type xcodeSim struct {
	mu          sync.Mutex
	exportLog   string
	exportExit  int
	exports     int
	widgetBuild string // CFBundleVersion of the extension; "" = same as the app
	number      string
}

func (x *xcodeSim) handle(c run.Cmd) (string, int, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	switch {
	case c.Name == "/usr/bin/xcode-select":
		return "/Applications/Xcode.app/Contents/Developer\n", 0, nil
	case c.Name == "/usr/bin/xcodebuild" && len(c.Args) > 0 && c.Args[0] == "archive":
		var archive string
		for i, a := range c.Args {
			if a == "-archivePath" {
				archive = c.Args[i+1]
			}
			if v, ok := strings.CutPrefix(a, "CURRENT_PROJECT_VERSION="); ok {
				x.number = v
			}
		}
		app := filepath.Join(archive, "Products", "Applications", "Driver.app")
		_ = os.MkdirAll(filepath.Join(app, "PlugIns", "W.appex"), 0o755)
		_ = os.WriteFile(filepath.Join(archive, "Info.plist"), []byte("x"), 0o644)
		_ = os.WriteFile(filepath.Join(app, "Info.plist"), []byte("app"), 0o644)
		_ = os.WriteFile(filepath.Join(app, "PlugIns", "W.appex", "Info.plist"), []byte("ext"), 0o644)
		return "** ARCHIVE SUCCEEDED **\n", 0, nil
	case c.Name == "/usr/bin/plutil":
		key, file := c.Args[1], c.Args[len(c.Args)-1]
		if key == "CFBundleShortVersionString" {
			return "1.0\n", 0, nil
		}
		if strings.Contains(file, "W.appex") && x.widgetBuild != "" {
			return x.widgetBuild + "\n", 0, nil
		}
		return x.number + "\n", 0, nil
	case c.Name == "/usr/bin/xcodebuild" && len(c.Args) > 0 && c.Args[0] == "-exportArchive":
		x.exports++
		return x.exportLog, x.exportExit, nil
	}
	return "", 0, nil
}

type harness struct {
	q    *queue.Queue
	st   *store.Store
	asc  *fakeASC
	sim  *xcodeSim
	tf   *Testflight
	repo string
}

func newHarness(t *testing.T, size SizeVerdict) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := filepath.Join(dir, "driver")
	_ = os.MkdirAll(filepath.Join(repo, "ios", "Driver.xcodeproj"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "eve.yml"), []byte("app: driver\nproject: {dir: ios, xcodeproj: Driver.xcodeproj, scheme: Driver}\n"), 0o644)
	cfg := &config.Server{DataDir: dir, Team: "2559B88H6C", Apps: map[string]config.App{
		"driver": {BundleID: "ai.selfdeprecated.driver", Testflight: config.TestflightConfig{Group: "Driver", Tester: "me@example.com"}},
	}}
	sim := &xcodeSim{exportLog: "** EXPORT SUCCEEDED **\n"}
	fa := &fakeASC{}
	d := &Deps{
		Config: cfg, Store: st, Runner: &run.Fake{Handler: sim.handle}, ASC: fa, Sources: fakeSources{},
		Size: fakeSize{size}, Classifier: fakeClassifier{},
		Sleep:           func(ctx context.Context, dur time.Duration) error { return ctx.Err() },
		ReconcileWindow: time.Millisecond, ReconcileInterval: time.Millisecond,
	}
	tf := NewTestflight(d)
	q := queue.New(st, dir, map[api.Verb]queue.Pipeline{api.VerbTestflight: tf}, nil, nil)
	return &harness{q: q, st: st, asc: fa, sim: sim, tf: tf, repo: repo}
}

func (h *harness) submit(t *testing.T, opts api.JobOptions) store.Job {
	t.Helper()
	j, _, err := h.st.CreateJob(context.Background(), store.Job{App: "driver", Verb: api.VerbTestflight, Request: api.JobRequest{
		App: "driver", Verb: api.VerbTestflight, Options: opts,
		Source: api.JobSource{Path: h.repo, Commit: "3f2c1ab", Stats: api.SourceStats{Bytes: 1000, Files: 10}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (h *harness) get(t *testing.T, id string) store.Job {
	t.Helper()
	j, err := h.st.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (h *harness) watch(t *testing.T, id string) store.Job {
	t.Helper()
	j := h.get(t, id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Sleep returns ctx.Err(), so give the watcher a context that is live but ends the loop
	// after the fake ASC's answers are used up: drive steps until terminal.
	for i := 0; i < 10 && !j.State.Terminal(); i++ {
		h.tf.Sleep = func(context.Context, time.Duration) error { return nil }
		jc := h.q.NewJobCtx(ctx, j)
		done := make(chan struct{})
		go func() { _ = h.tf.Watch(jc); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			cancel()
			<-done
		}
		j = h.get(t, id)
	}
	return j
}

func valid(state string) *asc.Build {
	return &asc.Build{
		ID: "b1", Number: "x", ProcessingState: "VALID", BuildAudienceType: "INTERNAL_ONLY",
		BetaDetail: &asc.BetaDetail{InternalBuildState: state},
	}
}

// --- tests ---

// Starts from processing, so it needs no build tools.
func TestDistributionIsReported(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	j := h.submit(t, api.JobOptions{})
	if err := h.q.NewJobCtx(context.Background(), j).EnterWith(api.StateProcessing, "uploaded", recordSource); err != nil {
		t.Fatal(err)
	}
	h.asc.builds = []*asc.Build{valid("IN_BETA_TESTING")}
	got := h.watch(t, j.ID)
	if got.State != api.StateSucceeded {
		t.Fatalf("final: %s %+v", got.State, got.Result.Failure)
	}
	want := api.Note{Kind: "distributed", Detail: "added to group Driver; What to Test set; me@example.com has accepted the TestFlight invite"}
	if len(got.Result.Notes) != 1 || got.Result.Notes[0] != want {
		t.Fatalf("notes: %+v", got.Result.Notes)
	}
	if got.Result.Phase != "" || got.Result.Source.Files != 10 || got.Result.Source.Bytes != 1000 {
		t.Fatalf("phase %q, source %+v", got.Result.Phase, got.Result.Source)
	}
}

func TestHappyPathToReadyForTesters(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	h.asc.latest = "202610161458"
	h.asc.invite = asc.InvitePending
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)

	got := h.get(t, j.ID)
	if got.State != api.StateProcessing {
		t.Fatalf("after delivery: %s %+v", got.State, got.Result.Failure)
	}
	if got.BuildVersion != "1.0" || got.UploadIntent == "" || got.Result.Size == nil {
		t.Fatalf("job not recorded: %+v", got)
	}
	if n := got.BuildNumber; n < "202610161459" {
		t.Fatalf("build number %s must be above App Store Connect's latest", n)
	}
	exp, _ := os.ReadFile(filepath.Join(h.q.DataDir, "jobs", j.ID, "ExportOptions.plist"))
	if !strings.Contains(string(exp), "<key>testFlightInternalTestingOnly</key><true/>") || !strings.Contains(string(exp), "<string>upload</string>") {
		t.Fatalf("export options: %s", exp)
	}

	h.asc.builds = []*asc.Build{{ID: "b1", ProcessingState: "PROCESSING"}, valid("IN_BETA_TESTING")}
	got = h.watch(t, j.ID)
	if got.State != api.StateSucceeded || got.Result.Build.ASCBuildID != "b1" {
		t.Fatalf("final: %s %+v", got.State, got.Result)
	}
	if len(h.asc.added) != 1 || !strings.Contains(h.asc.whatToTest, "3f2c1ab") {
		t.Fatalf("distribution: %v %q", h.asc.added, h.asc.whatToTest)
	}
	if n := got.Result.Notes; len(n) != 2 || n[0].Kind != "distributed" || n[0].Detail != "added to group Driver; What to Test set" ||
		n[1].Kind != api.HumanAcceptTestflightInvite {
		t.Fatalf("notes: %+v", got.Result.Notes)
	}
	if got.Result.Phase != "" || got.Result.Source.Files != 10 || got.Result.Source.Bytes != 1000 {
		t.Fatalf("phase %q, source %+v", got.Result.Phase, got.Result.Source)
	}
	if b, _ := h.st.Baseline(context.Background(), "driver"); b == nil || b.JobID != j.ID {
		t.Fatal("baseline not updated on success")
	}
	if s, _ := h.st.SourceStats(context.Background(), "driver"); s == nil || s.Bytes != 1000 {
		t.Fatal("source stats not recorded")
	}

	// The next build records which job it was measured against: once it succeeds it becomes
	// the baseline itself, and `eve size` still has to diff it against this one.
	next := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), next)
	if got := h.get(t, next.ID); got.Result.Size == nil || got.Result.Size.BaselineJob != j.ID {
		t.Fatalf("baseline job not recorded: %+v", got.Result.Size)
	}
}

func TestSizeGuardPauseThenConfirmSkipsRebuild(t *testing.T) {
	trip := SizeVerdict{Trip: true, Report: api.SizeReport{Summary: "214.6 MB (+166.4 MB)", Reasons: []string{"repository content: docs/"}}}
	h := newHarness(t, trip)
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)
	got := h.get(t, j.ID)
	if got.State != api.StateAwaitingConfirmation || got.Result.Size.Gate != api.GateAwaitingConfirmation || got.DeadlineAt.IsZero() {
		t.Fatalf("expected pause: %s %+v", got.State, got.Result.Size)
	}
	if h.sim.exports != 0 {
		t.Fatal("nothing may be uploaded before confirmation")
	}
	if _, err := h.q.Confirm(context.Background(), j.ID, "added onboarding videos"); err != nil {
		t.Fatal(err)
	}
	f := h.tf.Runner.(*run.Fake)
	archivesBefore := countArchives(f)
	h.q.Exec(context.Background(), h.get(t, j.ID))
	got = h.get(t, j.ID)
	if got.State != api.StateProcessing || h.sim.exports != 1 {
		t.Fatalf("after confirm: %s exports=%d %+v", got.State, h.sim.exports, got.Result.Failure)
	}
	if countArchives(f) != archivesBefore {
		t.Fatal("confirmation must not rebuild the archive")
	}
}

func countArchives(f *run.Fake) int {
	n := 0
	for _, c := range f.Commands() {
		if strings.HasPrefix(c, "/usr/bin/xcodebuild archive") {
			n++
		}
	}
	return n
}

func TestSecretInBundleFailsHard(t *testing.T) {
	h := newHarness(t, SizeVerdict{Secret: true, Trip: true, Report: api.SizeReport{Summary: "x", Reasons: []string{"AuthKey_J9584QW2TF.p8 in bundle"}}})
	j := h.submit(t, api.JobOptions{ExpectSizeChange: "videos"})
	h.q.Exec(context.Background(), j)
	got := h.get(t, j.ID)
	if got.State != api.StateFailed || got.Result.Failure.Class != "bundle.secret_material" || h.sim.exports != 0 {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
}

func TestExpectSizeChangeSkipsPause(t *testing.T) {
	h := newHarness(t, SizeVerdict{Trip: true, Report: api.SizeReport{Summary: "+18 MB"}})
	j := h.submit(t, api.JobOptions{ExpectSizeChange: "added onboarding videos"})
	h.q.Exec(context.Background(), j)
	if s := h.get(t, j.ID).State; s != api.StateProcessing {
		t.Fatal(s)
	}
}

func TestDuplicateUploadIsFollowedNotFailed(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	h.sim.exportLog, h.sim.exportExit = "ERROR ITMS-90189: Redundant Binary Upload\n** EXPORT FAILED **\n", 70
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)
	if s := h.get(t, j.ID).State; s != api.StateProcessing {
		t.Fatal(s)
	}
}

func TestClassifiedExportFailureDoesNotReconcile(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	h.sim.exportLog, h.sim.exportExit = "error: exportArchive Cloud signing permission error\n", 70
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)
	got := h.get(t, j.ID)
	if got.State != api.StateFailed || got.Result.Failure.Class != "signing.cloud_permission" || h.asc.reconN != 0 {
		t.Fatalf("%s %+v recon=%d", got.State, got.Result.Failure, h.asc.reconN)
	}
	if !strings.Contains(got.Result.Failure.Log, "--phase delivery") {
		t.Fatal(got.Result.Failure.Log)
	}
}

func TestAmbiguousExportReconcilesToProcessing(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	h.sim.exportLog, h.sim.exportExit = "something odd happened\n", 70
	h.asc.recon = asc.Reconciliation{Status: asc.UploadLive}
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)
	if s := h.get(t, j.ID).State; s != api.StateProcessing || h.asc.reconN != 1 || h.sim.exports != 1 {
		t.Fatalf("%s recon=%d exports=%d", s, h.asc.reconN, h.sim.exports)
	}
}

func TestAmbiguousExportNotVisibleRetriesOnce(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	h.sim.exportLog, h.sim.exportExit = "network went away\n", 70
	h.asc.recon = asc.Reconciliation{Status: asc.NotVisible}
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)
	got := h.get(t, j.ID)
	if got.State != api.StateFailed || h.sim.exports != 2 {
		t.Fatalf("want exactly one retry then failure: %s exports=%d", got.State, h.sim.exports)
	}
}

func TestCrashDuringDeliveryReconcilesInsteadOfUploadingAgain(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j) // ends parked in processing
	ctx := context.Background()
	// Simulate: the server died while delivering, before the job reached processing.
	_, _ = h.st.Update(ctx, j.ID, func(j *store.Job) { j.State, j.Phase = api.StateDelivering, api.StateDelivering })
	if err := h.q.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	h.asc.recon = asc.Reconciliation{Status: asc.Found}
	exportsBefore := h.sim.exports
	h.q.Exec(ctx, h.get(t, j.ID))
	got := h.get(t, j.ID)
	if got.State != api.StateProcessing || h.sim.exports != exportsBefore || h.asc.reconN != 1 {
		t.Fatalf("%s exports %d→%d recon=%d", got.State, exportsBefore, h.sim.exports, h.asc.reconN)
	}
}

func TestBuildNumberMismatchFails(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	h.sim.widgetBuild = "1"
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)
	got := h.get(t, j.ID)
	if got.State != api.StateFailed || got.Result.Failure.Class != "build.number_mismatch" {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
}

func TestRepoAppMismatchIsConfigError(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	_ = os.WriteFile(filepath.Join(h.repo, "eve.yml"), []byte("app: lineage\nproject: {dir: ios, xcodeproj: Driver.xcodeproj, scheme: Driver}\n"), 0o644)
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)
	got := h.get(t, j.ID)
	if got.State != api.StateFailed || got.Result.Failure.Class != "config" {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
}

func TestWatchOutcomes(t *testing.T) {
	cases := map[string]struct {
		build *asc.Build
		state api.State
	}{
		"rejected":          {&asc.Build{ID: "b", ProcessingState: "INVALID"}, api.StateRejected},
		"export compliance": {valid("MISSING_EXPORT_COMPLIANCE"), api.StateNeedsHuman},
		"expired":           {valid("EXPIRED"), api.StateFailed},
		"store eligible":    {&asc.Build{ID: "b", ProcessingState: "VALID", BuildAudienceType: "APP_STORE_ELIGIBLE", BetaDetail: &asc.BetaDetail{InternalBuildState: "READY_FOR_BETA_TESTING"}}, api.StateFailed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, SizeVerdict{})
			j := h.submit(t, api.JobOptions{})
			h.q.Exec(context.Background(), j)
			h.asc.builds = []*asc.Build{c.build}
			if got := h.watch(t, j.ID); got.State != c.state {
				t.Fatalf("%s %+v %+v", got.State, got.Result.Failure, got.Result.Human)
			}
		})
	}
}

func TestWatchDeadline(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)
	_, _ = h.st.Update(context.Background(), j.ID, func(j *store.Job) { j.DeadlineAt = time.Now().Add(-time.Minute) })
	if got := h.watch(t, j.ID); got.State != api.StateFailed || got.Result.Failure.Class != "asc.processing_deadline" {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
}

func TestChildEnvironmentInCommands(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)
	c, err := h.tf.Runner.(*run.Fake).Find("xcodebuild archive")
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(c.Env, " ")
	if !strings.Contains(env, "PATH=/usr/bin:/bin:/usr/sbin:/sbin") || !strings.Contains(env, "DEVELOPER_DIR=/Applications/Xcode.app") || strings.Contains(env, "SDKROOT") {
		t.Fatalf("env: %s", env)
	}
	if c.Dir == "" || !strings.HasSuffix(c.Dir, filepath.Join("src", "ios")) {
		t.Fatalf("archive must run in the job's copy of the project dir, got %q", c.Dir)
	}
}

func TestXcodeSignInBecomesNeedsHuman(t *testing.T) {
	h := newHarness(t, SizeVerdict{})
	h.sim.exportLog, h.sim.exportExit = "error: Your session has expired. Please log in.\n", 70
	j := h.submit(t, api.JobOptions{})
	h.q.Exec(context.Background(), j)
	got := h.get(t, j.ID)
	if got.State != api.StateNeedsHuman || got.Result.Human.Action != api.HumanXcodeSignIn || got.Result.Failure.Class != "xcode.account" {
		t.Fatalf("%s %+v %+v", got.State, got.Result.Human, got.Result.Failure)
	}
}
