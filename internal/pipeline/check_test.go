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
	mu       sync.Mutex
	summary  string // xcresulttool's JSON; "" = the test run wrote no result bundle
	testExit int
	testLog  string
	orphans  []string
	created  []string
	deleted  []string
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
			return `{"devices":{"iOS-26-5":[{"udid":"KEEP","name":"My iPhone"}` + strings.Repeat(",", min(len(ds), 1)) + strings.Join(ds, ",") + `]}}`, 0, nil
		case "create":
			s.created = append(s.created, a[2]+" "+a[3]+" "+a[4])
			return simUDID + "\n", 0, nil
		case "delete":
			s.deleted = append(s.deleted, a[2])
		}
		return "", 0, nil
	case c.Name == "/usr/bin/xcodebuild" && a[0] == "test":
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
	if !slices.Equal(sim.deleted, []string{"ORPHAN-1", simUDID}) {
		t.Fatalf("orphans and the job's Simulator must be deleted, and nothing else: %v", sim.deleted)
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
	if !slices.Contains(sim.deleted, simUDID) {
		t.Fatal("the Simulator must be deleted after a failed run")
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
	if !strings.Contains(got.Result.Failure.Log, "--phase test") || !slices.Contains(sim.deleted, simUDID) {
		t.Fatalf("%+v %v", got.Result.Failure, sim.deleted)
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
