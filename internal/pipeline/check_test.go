package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/store"
)

const simUDID = "0825B3D6-A17B-49A1-AC3C-0DADB895B9F8"

// simctlSim answers like simctl, xcodebuild test and xcresulttool.
type simctlSim struct {
	mu              sync.Mutex
	summary         string // xcresulttool's JSON; "" = the test run wrote no result bundle
	testExit        int
	testLog         string
	orphans         []string
	created         []string
	shutdown        []string
	name            string
	state           string
	bootFailure     string
	shutdownFailure bool
	onBoot          func()
	onCreate        func()
	onTest          func()
}

const runtimesJSON = `{"runtimes":[
 {"identifier":"com.apple.CoreSimulator.SimRuntime.iOS-18-0","platform":"iOS","version":"18.0","isAvailable":true,
  "supportedDeviceTypes":[{"identifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-16","name":"iPhone 16","productFamily":"iPhone"}]},
 {"identifier":"com.apple.CoreSimulator.SimRuntime.iOS-26-5","platform":"iOS","version":"26.5","isAvailable":true,
  "supportedDeviceTypes":[
   {"identifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro-Max","name":"iPhone 17 Pro Max","productFamily":"iPhone"},
   {"identifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro","name":"iPhone 17 Pro","productFamily":"iPhone"},
   {"identifier":"com.apple.CoreSimulator.SimDeviceType.iPad-Pro-13","name":"iPad Pro 13-inch","productFamily":"iPad"},
   {"identifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-11","name":"iPhone 11","productFamily":"iPhone"}]}]}`

func (s *simctlSim) handle(c run.Cmd) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := c.Args
	switch {
	case c.Name == "/usr/bin/xcode-select":
		return "/Applications/Xcode.app/Contents/Developer\n", 0, nil
	case c.Name == "/usr/bin/xcrun" && a[0] == "simctl":
		switch a[1] {
		case "list":
			if a[3] == "runtimes" {
				return runtimesJSON, 0, nil
			}
			var ds []string
			for _, o := range s.orphans {
				ds = append(ds, `{"udid":"`+o+`","name":"eve-j_old"}`)
			}
			if s.name != "" {
				ds = append(ds, `{"udid":"`+simUDID+`","name":"`+s.name+`","state":"`+s.state+`"}`)
			}
			return `{"devices":{"iOS-26-5":[{"udid":"KEEP","name":"My iPhone","state":"Booted"}` + strings.Repeat(",", min(len(ds), 1)) + strings.Join(ds, ",") + `]}}`, 0, nil
		case "create":
			s.created = append(s.created, a[2]+" "+a[3]+" "+a[4])
			s.name, s.state = a[2], "Shutdown"
			if s.onCreate != nil {
				s.onCreate()
			}
			return simUDID + "\n", 0, nil
		case "boot", "bootstatus":
			if s.onBoot != nil {
				s.onBoot()
			}
			s.state = "Booted"
			if s.bootFailure == a[1] {
				return "boot failed", 1, nil
			}
		case "shutdown":
			s.shutdown = append(s.shutdown, a[2])
			if s.shutdownFailure {
				return "CoreSimulator busy", 1, nil
			}
			s.state = "Shutdown"
		case "delete", "erase":
			panic("must never erase/delete Simulator data")
		}
		return "", 0, nil
	case c.Name == "/usr/bin/xcodebuild" && a[0] == "test":
		if s.onTest != nil {
			s.onTest()
		}
		if s.summary != "" {
			for i, x := range a {
				if x == "-resultBundlePath" {
					_ = os.MkdirAll(a[i+1], 0o755)
					_ = os.WriteFile(filepath.Join(a[i+1], "Info.plist"), []byte("x"), 0o644)
				}
			}
		}
		return s.testLog, s.testExit, nil
	case c.Name == "/usr/bin/xcrun" && a[0] == "xcresulttool":
		return s.summary, 0, nil
	}
	return "", 0, nil
}

func newCheckHarness(t *testing.T, eveYML string, sim *simctlSim) (*queue.Queue, *store.Store, *run.Fake, store.Job) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := filepath.Join(dir, "lineage")
	_ = os.MkdirAll(filepath.Join(repo, "Lineage.xcodeproj"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "eve.yml"), []byte(eveYML), 0o644)
	cfg := &config.Server{DataDir: dir, Apps: map[string]config.App{"lineage": {BundleID: "ai.selfdeprecated.lineage"}}}
	f := &run.Fake{Handler: sim.handle}
	d := &Deps{Config: cfg, Store: st, Runner: f, Sources: fakeSources{}, Classifier: fakeClassifier{}}
	q := queue.New(st, dir, map[api.Verb]queue.Pipeline{api.VerbCheck: NewCheck(d)}, nil, nil)
	j, _, err := st.CreateJob(context.Background(), store.Job{App: "lineage", Verb: api.VerbCheck, Request: api.JobRequest{
		App: "lineage", Verb: api.VerbCheck, Source: api.JobSource{Path: repo, Commit: "abc1234"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return q, st, f, j
}

const lineageYML = "app: lineage\nproject: {xcodeproj: Lineage.xcodeproj, scheme: Lineage}\n"

func TestCheckBuildsForTheSimulatorSignedAdHoc(t *testing.T) {
	sim := &simctlSim{}
	q, st, f, j := newCheckHarness(t, lineageYML, sim)
	q.Exec(context.Background(), j)
	got, _ := st.Get(context.Background(), j.ID)
	if got.State != api.StateSucceeded || got.Result.Tests != nil || got.Result.Source.Commit != "abc1234" {
		t.Fatalf("%s %+v", got.State, got.Result)
	}
	c, err := f.Find("xcodebuild build")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Args, "generic/platform=iOS Simulator") || !slices.Contains(c.Args, "CODE_SIGN_IDENTITY=-") {
		t.Fatalf("not an ad-hoc signed Simulator build: %v", c.Args)
	}
	if _, err := f.Find("simctl"); err == nil {
		t.Fatal("a build-only check must not create a Simulator")
	}
}

func TestCheckRunsTestsOnAFreshSimulator(t *testing.T) {
	sim := &simctlSim{orphans: []string{"ORPHAN-1"}, summary: `{"result":"Passed","totalTestCount":75,"passedTests":74,"skippedTests":1,"failedTests":0,"testFailures":[]}`}
	q, st, f, j := newCheckHarness(t, lineageYML+"check: {test: true}\n", sim)
	q.Exec(context.Background(), j)
	got, _ := st.Get(context.Background(), j.ID)
	if got.State != api.StateSucceeded {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
	tests := got.Result.Tests
	if tests == nil || tests.Passed != 74 || tests.Skipped != 1 || tests.Total != 75 || tests.Simulator != "iPhone 17 Pro, iOS 26.5" {
		t.Fatalf("tests: %+v", tests)
	}
	if len(sim.created) != 1 || !strings.HasPrefix(sim.created[0], "eve-"+j.ID+" com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro com.apple.CoreSimulator.SimRuntime.iOS-26-5") {
		t.Fatalf("created: %v", sim.created)
	}
	if !slices.Equal(sim.shutdown, []string{simUDID}) {
		t.Fatalf("only the registered job Simulator may shut down: %v", sim.shutdown)
	}
	c, _ := f.Find("xcodebuild test")
	if !slices.Contains(c.Args, "platform=iOS Simulator,id="+simUDID) || !slices.Contains(c.Args, "CODE_SIGN_IDENTITY=-") ||
		!strings.Contains(c.String(), "-collect-test-diagnostics never") {
		t.Fatalf("test args: %v", c.Args)
	}
}

func TestCheckReportsTheFirstFailingTests(t *testing.T) {
	sim := &simctlSim{testExit: 65, testLog: "** TEST FAILED **\n", summary: `{"result":"Failed","totalTestCount":3,"passedTests":1,"failedTests":2,
		"testFailures":[
		 {"testName":"testOnboarding()","targetName":"LineageUITests","testIdentifierString":"LineageUITests/testOnboarding()",
		  "failureText":"XCTAssertTrue failed - \"Continue\" button\n  not found"},
		 {"testName":"testSync()","targetName":"LineageTests","failureText":"timed out"}]}`}
	q, st, _, j := newCheckHarness(t, lineageYML+"check: {test: true, simulator: iPhone 11}\n", sim)
	q.Exec(context.Background(), j)
	got, _ := st.Get(context.Background(), j.ID)
	f := got.Result.Failure
	if got.State != api.StateFailed || f == nil || f.Class != "test.failed" {
		t.Fatalf("%s %+v", got.State, f)
	}
	want := `2 of 3 tests failed; first: LineageUITests/testOnboarding(): XCTAssertTrue failed - "Continue" button not found`
	if f.Summary != want || len(f.Excerpt) != 2 || f.Excerpt[1] != "LineageTests/testSync(): timed out" {
		t.Fatalf("summary %q excerpt %q", f.Summary, f.Excerpt)
	}
	if got.Result.Tests == nil || got.Result.Tests.Failed != 2 || got.Result.Tests.Simulator != "iPhone 11, iOS 26.5" {
		t.Fatalf("tests: %+v", got.Result.Tests)
	}
	if !slices.Contains(sim.shutdown, simUDID) {
		t.Fatal("the Simulator must shut down after a failed run")
	}
}

func TestCheckTestsThatNeverRanAreClassifiedFromTheLog(t *testing.T) {
	sim := &simctlSim{testExit: 65, testLog: "error: cannot find 'Foo' in scope\n** TEST FAILED **\n"}
	q, st, _, j := newCheckHarness(t, lineageYML+"check: {test: true}\n", sim)
	q.Exec(context.Background(), j)
	got, _ := st.Get(context.Background(), j.ID)
	if got.State != api.StateFailed || got.Result.Failure == nil || got.Result.Failure.Class != "unknown" || got.Result.Tests != nil {
		t.Fatalf("%s %+v", got.State, got.Result)
	}
	if !strings.Contains(got.Result.Failure.Log, "--phase test") || !slices.Contains(sim.shutdown, simUDID) {
		t.Fatalf("%+v %v", got.Result.Failure, sim.shutdown)
	}
}

func TestCheckUnknownSimulatorIsReported(t *testing.T) {
	q, st, _, j := newCheckHarness(t, lineageYML+"check: {test: true, simulator: iPhone 99}\n", &simctlSim{})
	q.Exec(context.Background(), j)
	got, _ := st.Get(context.Background(), j.ID)
	if f := got.Result.Failure; f == nil || f.Class != "simulator.unavailable" || !strings.Contains(f.Summary, `"iPhone 99"`) {
		t.Fatalf("%+v", got.Result.Failure)
	}
}

func TestCheckRegistersLeaseBeforeBootAndDuringQuietTests(t *testing.T) {
	sim := &simctlSim{summary: `{"result":"Passed","totalTestCount":1,"passedTests":1}`}
	q, st, _, j := newCheckHarness(t, lineageYML+"check: {test: true}\n", sim)
	assertLeased := func() {
		ds, err := st.Simulators(context.Background())
		if err != nil || len(ds) != 1 || ds[0].UDID != simUDID || ds[0].JobID != j.ID || !ds[0].ReleasedAt.IsZero() {
			t.Fatalf("missing lease before/during use: %+v %v", ds, err)
		}
	}
	sim.onBoot, sim.onTest = assertLeased, assertLeased
	q.Exec(context.Background(), j)
	ds, _ := st.Simulators(context.Background())
	if len(ds) != 1 || !ds[0].Settled {
		t.Fatalf("cleanup not settled: %+v", ds)
	}
	got, _ := st.Get(context.Background(), j.ID)
	if got.Pgid != 0 {
		t.Fatal("cleanup overwrote terminal process group", got.Pgid)
	}
}

func TestCheckCleansUpAfterPartialBootFailure(t *testing.T) {
	for _, phase := range []string{"boot", "bootstatus"} {
		t.Run(phase, func(t *testing.T) {
			sim := &simctlSim{bootFailure: phase}
			q, st, _, j := newCheckHarness(t, lineageYML+"check: {test: true}\n", sim)
			q.Exec(context.Background(), j)
			got, _ := st.Get(context.Background(), j.ID)
			if got.State != api.StateFailed || got.Result.Failure.Class != "simulator.unavailable" || !slices.Equal(sim.shutdown, []string{simUDID}) {
				t.Fatalf("%+v shutdown=%v", got.Result, sim.shutdown)
			}
		})
	}
}

func TestCheckCleansUpWhenCancelledOrServerStops(t *testing.T) {
	for _, byUser := range []bool{false, true} {
		t.Run(map[bool]string{true: "user cancel", false: "server stop"}[byUser], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sim := &simctlSim{}
			q, st, _, j := newCheckHarness(t, lineageYML+"check: {test: true}\n", sim)
			sim.onTest = func() {
				if byUser {
					if _, err := q.Cancel(ctx, j.ID, false); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
			}
			q.Exec(ctx, j)
			got, _ := st.Get(context.Background(), j.ID)
			if !slices.Equal(sim.shutdown, []string{simUDID}) {
				t.Fatal("cancelled context prevented teardown", sim.shutdown)
			}
			if byUser && got.State != api.StateCancelled {
				t.Fatal(got.State)
			}
			if !byUser && got.State.Terminal() {
				t.Fatal("server stop must leave job for recovery", got.State)
			}
		})
	}
}

func TestCheckCleanupFailureIsReportedWithoutChangingTestOutcome(t *testing.T) {
	sim := &simctlSim{shutdownFailure: true, summary: `{"result":"Passed","totalTestCount":1,"passedTests":1}`}
	q, st, _, j := newCheckHarness(t, lineageYML+"check: {test: true}\n", sim)
	q.Exec(context.Background(), j)
	got, _ := st.Get(context.Background(), j.ID)
	ds, _ := st.Simulators(context.Background())
	if got.State != api.StateSucceeded || len(got.Result.Notes) != 1 || got.Result.Notes[0].Kind != "simulator_cleanup" || len(ds) != 1 || ds[0].Settled || ds[0].ReleasedAt.IsZero() {
		t.Fatalf("result=%+v registry=%+v", got.Result, ds)
	}
}

func TestCheckNeverBootsWhenOwnershipCannotBeRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sim := &simctlSim{onCreate: cancel}
	q, st, f, j := newCheckHarness(t, lineageYML+"check: {test: true}\n", sim)
	q.Exec(ctx, j)
	if _, err := f.Find("simctl boot"); err == nil {
		t.Fatal("device booted without durable ownership")
	}
	ds, err := st.Simulators(context.Background())
	if err != nil || len(ds) != 0 || sim.state != "Shutdown" || len(sim.shutdown) != 0 {
		t.Fatalf("unregistered device must stay stopped: %+v %s %v", ds, sim.state, err)
	}
}

// An Expo app has no Xcode project until pre generates it: eve.yml names the workspace that
// `expo prebuild` and `pod install` produce, and the server's tool path supplies node and pod.
func TestCheckBuildsTheWorkspacePreGenerates(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := filepath.Join(dir, "approve-al")
	_ = os.MkdirAll(filepath.Join(repo, "apps", "mobile"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "eve.yml"), []byte(`app: approveal
project: {dir: apps/mobile/ios, xcworkspace: ApproveAL.xcworkspace, scheme: ApproveAL}
pre:
  - cd apps/mobile && expo prebuild --platform ios --no-install && cd ios && pod install
`), 0o644)
	cfg := &config.Server{
		DataDir: dir, Path: []string{"/nix/node/bin", "/nix/cocoapods/bin"},
		Apps: map[string]config.App{"approveal": {BundleID: "ai.selfdeprecated.approveal"}},
	}
	sim := &simctlSim{}
	f := &run.Fake{Handler: func(c run.Cmd) (string, int, error) {
		if c.Name == "/bin/zsh" {
			_ = os.MkdirAll(filepath.Join(c.Dir, "apps", "mobile", "ios", "ApproveAL.xcworkspace"), 0o755)
		}
		return sim.handle(c)
	}}
	d := &Deps{Config: cfg, Store: st, Runner: f, Sources: fakeSources{}, Classifier: fakeClassifier{}}
	q := queue.New(st, dir, map[api.Verb]queue.Pipeline{api.VerbCheck: NewCheck(d)}, nil, nil)
	j, _, err := st.CreateJob(context.Background(), store.Job{App: "approveal", Verb: api.VerbCheck, Request: api.JobRequest{
		App: "approveal", Verb: api.VerbCheck, Source: api.JobSource{Path: repo},
	}})
	if err != nil {
		t.Fatal(err)
	}
	q.Exec(context.Background(), j)
	got, _ := st.Get(context.Background(), j.ID)
	if got.State != api.StateSucceeded {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
	path := "PATH=/usr/bin:/bin:/usr/sbin:/sbin:/nix/node/bin:/nix/cocoapods/bin"
	pre, _ := f.Find("pod install")
	build, err := f.Find("xcodebuild build")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pre.Env, path) || !slices.Contains(build.Env, path) {
		t.Fatalf("tool path missing: pre %v, build %v", pre.Env, build.Env)
	}
	if !strings.Contains(build.String(), "-workspace ApproveAL.xcworkspace -scheme ApproveAL") || !strings.HasSuffix(build.Dir, filepath.Join("apps", "mobile", "ios")) {
		t.Fatalf("not a workspace build in the project dir: %s in %s", build.String(), build.Dir)
	}
}
