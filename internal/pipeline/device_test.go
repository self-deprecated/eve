package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/store"
)

const (
	phoneID   = "DE53C108-34AF-5BE1-969D-ECB9BDF3A38B"
	phoneUDID = "00008150-001C0CDE22F3C01C"
)

// devicesJSON is `devicectl list devices` with a Simulator, the paired iPhone and an iPad, in
// Xcode 27's shape (properties) for the first two and the deprecated one for the iPad.
const devicesJSON = `{"result":{"devices":[
 {"identifier":"A468A9B3-5983-44BA-9879-6EE373222224","properties":{"connection":{"pairingState":"paired"},
  "hardware":{"udid":"A468A9B3-5983-44BA-9879-6EE373222224","platform":"iOS","deviceType":"iPhone","reality":"simulated"},
  "state":{"name":"Driver"}}},
 {"identifier":"` + phoneID + `","properties":{"connection":{"pairingState":"paired","state":"disconnected"},
  "hardware":{"udid":"` + phoneUDID + `","platform":"iOS","deviceType":"iPhone"},
  "software":{"osVersionNumber":{"stringValue":"27.0.1"}},"state":{"name":"Jordan’s iPhone"}}},
 {"identifier":"IPAD-1","connectionProperties":{"pairingState":"paired"},
  "hardwareProperties":{"udid":"00008112-IPAD","platform":"iOS","deviceType":"iPad"},"deviceProperties":{"name":"iPad"}}]}}`

// Answers for install and launch: ok, or the failure devicectl prints.
const (
	ok     = ""
	away   = "ERROR: A connection to this device could not be established. (com.apple.dt.CoreDeviceError error 4000 (0x0FA0))"
	locked = `ERROR: The request was denied by service delegate (SBMainWorkspace) for reason: Locked ("Unable to launch ai.selfdeprecated.driver because the device was not, or could not be, unlocked").`
)

// deviceSim answers like xcodebuild and devicectl. install and launch are successive answers; the
// last one repeats.
type deviceSim struct {
	mu      sync.Mutex
	install []string
	launch  []string
	order   []string
}

func next(s *[]string) string {
	v := (*s)[0]
	if len(*s) > 1 {
		*s = (*s)[1:]
	}
	return v
}

func (s *deviceSim) handle(c run.Cmd) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := c.Args
	switch {
	case c.Name == "/usr/bin/xcode-select":
		return "/Applications/Xcode.app/Contents/Developer\n", 0, nil
	case c.Name == "/usr/bin/xcodebuild" && a[0] == "build":
		s.order = append(s.order, "build")
		i := slices.Index(a, "-derivedDataPath")
		app := filepath.Join(a[i+1], "Build", "Products", a[slices.Index(a, "-configuration")+1]+"-iphoneos", "Driver.app")
		_ = os.MkdirAll(app, 0o755)
		_ = os.WriteFile(filepath.Join(app, "Info.plist"), []byte("app"), 0o644)
		return "** BUILD SUCCEEDED **\n", 0, nil
	case c.Name == "/usr/bin/xcrun" && a[0] == "devicectl":
		switch strings.Join(a[1:3], " ") {
		case "list devices":
			return devicesJSON, 0, nil
		case "device install":
			s.order = append(s.order, "install")
			if v := next(&s.install); v != ok {
				return v, 1, nil
			}
			return "App installed:\n", 0, nil
		case "device process":
			s.order = append(s.order, "launch")
			if v := next(&s.launch); v != ok {
				return v, 1, nil
			}
			return "Launched application with ai.selfdeprecated.driver bundle identifier.\n", 0, nil
		}
	}
	return "", 0, nil
}

type deviceHarness struct {
	q    *queue.Queue
	st   *store.Store
	f    *run.Fake
	repo string
	now  time.Time
}

func newDeviceHarness(t *testing.T, sim *deviceSim) *deviceHarness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &deviceHarness{st: st, repo: filepath.Join(dir, "driver"), now: time.Unix(1_800_000_000, 0)}
	_ = os.MkdirAll(filepath.Join(h.repo, "ios", "Driver.xcodeproj"), 0o755)
	_ = os.WriteFile(filepath.Join(h.repo, "eve.yml"), []byte("app: driver\nproject: {dir: ios, xcodeproj: Driver.xcodeproj, scheme: Driver}\n"), 0o644)
	cfg := &config.Server{DataDir: dir, Apps: map[string]config.App{"driver": {BundleID: "ai.selfdeprecated.driver"}}}
	h.f = &run.Fake{Handler: sim.handle}
	d := &Deps{
		Config: cfg, Store: st, Runner: h.f, Sources: fakeSources{}, Classifier: fakeClassifier{},
		Now: func() time.Time { return h.now },
		// Each poll moves the clock on, so the ten-minute wait runs out in 120 polls.
		Sleep: func(ctx context.Context, dur time.Duration) error { h.now = h.now.Add(dur); return ctx.Err() },
	}
	h.q = queue.New(st, dir, map[api.Verb]queue.Pipeline{api.VerbDevice: NewDevice(d)}, nil, nil)
	return h
}

func (h *deviceHarness) run(t *testing.T, opts api.JobOptions) store.Job {
	t.Helper()
	j, _, err := h.st.CreateJob(context.Background(), store.Job{App: "driver", Verb: api.VerbDevice, Request: api.JobRequest{
		App: "driver", Verb: api.VerbDevice, Options: opts, Source: api.JobSource{Path: h.repo},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.q.Exec(context.Background(), j)
	got, _ := h.st.Get(context.Background(), j.ID)
	return got
}

func (h *deviceHarness) waits(t *testing.T, id string) int {
	t.Helper()
	evs, err := h.st.Events(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Kind == api.EventTransition && e.State == api.StateWaitingForDevice {
			n++
		}
	}
	return n
}

func TestDeviceBuildsInstallsAndLaunchesOnThePairedIPhone(t *testing.T) {
	sim := &deviceSim{install: []string{ok}, launch: []string{ok}}
	h := newDeviceHarness(t, sim)
	got := h.run(t, api.JobOptions{})
	if got.State != api.StateSucceeded {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
	if n := got.Result.Notes; len(n) != 1 || n[0].Kind != "launched" || n[0].Detail != "Jordan’s iPhone, iOS 27.0.1" {
		t.Fatalf("notes %+v", n)
	}
	if !slices.Equal(sim.order, []string{"build", "install", "launch"}) || h.waits(t, got.ID) != 0 {
		t.Fatalf("order %v: no checks and no wait when the phone takes it", sim.order)
	}
	b, _ := h.f.Find("xcodebuild build")
	if !slices.Contains(b.Args, "generic/platform=iOS") || !slices.Contains(b.Args, "Debug") || !slices.Contains(b.Args, "-allowProvisioningUpdates") {
		t.Fatalf("build args %v", b.Args)
	}
	in, _ := h.f.Find("devicectl device install")
	if !slices.Contains(in.Args, phoneID) || !strings.HasSuffix(in.Args[len(in.Args)-1], filepath.Join("device", "Driver.app")) {
		t.Fatalf("install %v: the job's kept copy goes to the phone", in.Args)
	}
	l, _ := h.f.Find("devicectl device process launch")
	if !slices.Contains(l.Args, "--terminate-existing") || l.Args[len(l.Args)-1] != "ai.selfdeprecated.driver" {
		t.Fatalf("launch %v", l.Args)
	}
}

func TestDeviceWaitsOnlyWhenThePhoneSaysSo(t *testing.T) {
	sim := &deviceSim{install: []string{away, away, ok}, launch: []string{away, away, ok}}
	h := newDeviceHarness(t, sim)
	got := h.run(t, api.JobOptions{})
	if got.State != api.StateSucceeded {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
	want := []string{"build", "install", "install", "install", "launch", "launch", "launch"}
	if !slices.Equal(sim.order, want) {
		t.Fatalf("order %v, want %v: build, then retry each step until the phone takes it", sim.order, want)
	}
	if n := h.waits(t, got.ID); n != 1 {
		t.Fatalf("entered waiting_for_device %d times, want once (it notifies)", n)
	}
}

func TestDeviceLockedPhoneGetsTheInstallWithoutAWait(t *testing.T) {
	sim := &deviceSim{install: []string{ok}, launch: []string{locked}}
	h := newDeviceHarness(t, sim)
	got := h.run(t, api.JobOptions{})
	if got.State != api.StateSucceeded || !slices.Equal(sim.order, []string{"build", "install", "launch"}) || h.waits(t, got.ID) != 0 {
		t.Fatalf("%s order %v: installed, not launched, no wait", got.State, sim.order)
	}
	if n := got.Result.Notes; len(n) != 1 || n[0].Kind != "not_launched" || !strings.Contains(n[0].Detail, "Jordan’s iPhone is locked") {
		t.Fatalf("notes %+v", n)
	}
}

func TestDeviceSaysWhenQuartzCannotReachThePhone(t *testing.T) {
	h := newDeviceHarness(t, &deviceSim{install: []string{away}, launch: []string{ok}})
	got := h.run(t, api.JobOptions{})
	if got.State != api.StateNeedsHuman || got.Result.Human == nil || got.Result.Human.Action != api.HumanConnectPhone {
		t.Fatalf("%s %+v", got.State, got.Result.Human)
	}
	if !strings.Contains(got.Result.Human.Detail, "Jordan’s iPhone: Quartz can't reach the phone: its Wi-Fi is off") ||
		!strings.Contains(got.Result.Human.Detail, "gave up after 10m0s") {
		t.Fatalf("detail %q", got.Result.Human.Detail)
	}
	if f := got.Result.Failure; f == nil || f.Class != "device.unavailable" {
		t.Fatalf("failure %+v", f)
	}
}

func TestDeviceOtherFailuresFailAtOnce(t *testing.T) {
	sim := &deviceSim{install: []string{"ERROR: The application's signature is invalid"}, launch: []string{ok}}
	h := newDeviceHarness(t, sim)
	got := h.run(t, api.JobOptions{})
	if got.State != api.StateFailed || !slices.Equal(sim.order, []string{"build", "install"}) {
		t.Fatalf("%s order %v", got.State, sim.order)
	}
}

func TestDeviceClassifiesOnlyTheLatestAttempt(t *testing.T) {
	sim := &deviceSim{install: []string{away, "ERROR: The application's signature is invalid"}, launch: []string{ok}}
	h := newDeviceHarness(t, sim)
	got := h.run(t, api.JobOptions{})
	if got.State != api.StateFailed || !slices.Equal(sim.order, []string{"build", "install", "install"}) {
		t.Fatalf("%s order %v: the second attempt's real failure must end the job", got.State, sim.order)
	}
	if f := got.Result.Failure; f == nil || f.Class != "unknown" || slices.ContainsFunc(f.Excerpt, func(l string) bool { return strings.Contains(l, "could not be established") }) {
		t.Fatalf("failure %+v: only the last attempt's output", f)
	}
}

func TestDeviceUnknownNameListsThePhysicalDevices(t *testing.T) {
	h := newDeviceHarness(t, &deviceSim{install: []string{ok}, launch: []string{ok}})
	got := h.run(t, api.JobOptions{Device: "Work Phone"})
	f := got.Result.Failure
	if got.State != api.StateFailed || f == nil || f.Class != "device.unknown" || f.Summary != `no paired device matches "Work Phone"; physical devices: Jordan’s iPhone, iPad` {
		t.Fatalf("%s %+v", got.State, f)
	}
	if _, err := h.f.Find("xcodebuild build"); err == nil {
		t.Fatal("no build for a device that isn't there")
	}
}

func TestDeviceMatchesByUDIDAndBuildsRelease(t *testing.T) {
	h := newDeviceHarness(t, &deviceSim{install: []string{ok}, launch: []string{ok}})
	got := h.run(t, api.JobOptions{Device: strings.ToLower(phoneUDID), Release: true})
	if got.State != api.StateSucceeded {
		t.Fatalf("%s %+v", got.State, got.Result.Failure)
	}
	if b, _ := h.f.Find("xcodebuild build"); !slices.Contains(b.Args, "Release") {
		t.Fatalf("build args %v", b.Args)
	}
}

func TestDeviceResumesAtInstallAfterARestart(t *testing.T) {
	sim := &deviceSim{install: []string{ok}, launch: []string{ok}}
	h := newDeviceHarness(t, sim)
	j, _, _ := h.st.CreateJob(context.Background(), store.Job{App: "driver", Verb: api.VerbDevice, Request: api.JobRequest{
		App: "driver", Verb: api.VerbDevice, Source: api.JobSource{Path: h.repo},
	}})
	kept := filepath.Join(h.q.NewJobCtx(context.Background(), j).Dir, "device", "Driver.app")
	_ = os.MkdirAll(kept, 0o755)
	_ = os.WriteFile(filepath.Join(kept, "Info.plist"), []byte("app"), 0o644)
	j, _ = h.st.Update(context.Background(), j.ID, func(j *store.Job) { j.ResumeFrom = api.StateWaitingForDevice })
	h.q.Exec(context.Background(), j)
	got, _ := h.st.Get(context.Background(), j.ID)
	if got.State != api.StateSucceeded || !slices.Equal(sim.order, []string{"install", "launch"}) {
		t.Fatalf("%s order %v: the kept build is installed without rebuilding", got.State, sim.order)
	}
}
