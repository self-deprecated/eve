package classify

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/self-deprecated/eve/internal/api"
)

// Fixture directories that are not classes: real logs that must never be given a class.
const (
	dirSuccess = "success" // logs of runs that succeeded
	dirUnknown = "unknown" // real failures no rule explains (yet)
)

type fixture struct {
	class, name, path string
	log               []byte
}

func fixtures(t *testing.T) []fixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "logs", "*", "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no fixtures")
	}
	var out []fixture
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fixture{
			class: filepath.Base(filepath.Dir(p)),
			name:  strings.TrimSuffix(filepath.Base(p), ".log"),
			path:  p,
			log:   b,
		})
	}
	return out
}

// phaseFor is the phase a log of class would come from.
func phaseFor(class string) api.State {
	switch {
	case class == ClassTestsPre:
		return api.StatePreparing
	case class == ClassTestsFailed:
		return api.StateTesting
	case strings.HasPrefix(class, "device."):
		return api.StateLaunching
	case strings.HasPrefix(class, "upload."), strings.HasPrefix(class, "asc."),
		class == ClassEnvRsync, class == ClassSigningCloudPermission, class == ClassXcodeAccount:
		return api.StateDelivering
	}
	return api.StateBuilding
}

var allPhases = []api.State{
	"", api.StatePreparing, api.StateBuilding, api.StateArchiving, api.StateDelivering,
	api.StateProcessing, api.StateInstalling, api.StateLaunching, api.StateTesting,
}

func TestFixtures(t *testing.T) {
	for _, f := range fixtures(t) {
		t.Run(f.class+"/"+f.name, func(t *testing.T) {
			if f.class == dirSuccess || f.class == dirUnknown {
				for _, p := range allPhases {
					got := Classify(Input{Phase: p, Log: f.log, ExitCode: 65})
					if got.Class != ClassUnknown {
						t.Fatalf("phase %q: classified as %s (%s); excerpt %q", p, got.Class, got.Summary, got.Excerpt)
					}
					checkFailure(t, got)
				}
				if IsDuplicateUpload(f.log) {
					t.Fatal("IsDuplicateUpload on a log that isn't one")
				}
				return
			}
			got := Classify(Input{Phase: phaseFor(f.class), Log: f.log, ExitCode: 65})
			if got.Class != f.class {
				t.Fatalf("got %s (%s), want %s; excerpt %q", got.Class, got.Summary, f.class, got.Excerpt)
			}
			checkFailure(t, got)
			if got.By != "rules" || got.Log != "" {
				t.Fatalf("By=%q Log=%q", got.By, got.Log)
			}
			if dup := IsDuplicateUpload(f.log); dup != (f.class == ClassUploadDuplicate) {
				t.Fatalf("IsDuplicateUpload = %v", dup)
			}
		})
	}
}

func checkFailure(t *testing.T, f *api.Failure) {
	t.Helper()
	if f.Summary == "" || f.Hint == "" {
		t.Fatalf("empty summary or hint: %+v", f)
	}
	if len(f.Excerpt) == 0 || len(f.Excerpt) > MaxExcerptLines {
		t.Fatalf("excerpt has %d lines", len(f.Excerpt))
	}
	if size(f.Excerpt) > MaxExcerptBytes+MaxExcerptLineLen*4 {
		t.Fatalf("excerpt is %d bytes", size(f.Excerpt))
	}
	for _, l := range f.Excerpt {
		if n := utf8.RuneCountInString(l); n > MaxExcerptLineLen || !utf8.ValidString(l) || l == "" {
			t.Fatalf("bad excerpt line (%d runes): %q", n, l)
		}
	}
}

// TestDetails pins what the rules extract from specific real and synthetic logs.
func TestDetails(t *testing.T) {
	cases := []struct {
		path    string
		ctx     map[string]string
		phase   api.State
		summary []string
		hint    []string
		human   string
	}{
		{path: "upload.purpose_string/driver-camera.log", hint: []string{"NSCameraUsageDescription"}},
		{path: "upload.purpose_string/asc-processing-two-keys.log", summary: []string{"NSMicrophoneUsageDescription, NSMotionUsageDescription"}},
		{path: "upload.missing_icon/screengate-export.log", summary: []string{"152x152", "120x120"}},
		{path: "tests.failed/screengate-xcuitest.log", summary: []string{"GateFlowTests.testUnlockShowsGate", "XCTAssertTrue failed - Gate button did not appear"}, hint: []string{"UITests/GateFlowTests.swift:42"}},
		{path: "device.locked/bodydiff-launch.log", summary: []string{"locked"}},
		{path: "tests.failed/lineage-island-swift-testing.log", summary: []string{"soakStaysFiniteAndInBounds()", "Expectation failed"}, hint: []string{"IslandPresentationTests.swift:231:9"}},
		{path: "tests.pre/lineage-swift-test.log", phase: api.StatePreparing, summary: []string{"swiftReplaysTypeScriptHistoriesToIdenticalWorldsAndExplanations()"}},
		{path: "build.compile/bodydiff-camera.log", summary: []string{"Camera.swift:158:", "cannot infer contextual base"}},
		{path: "build.compile/lineage-session-ansi.log", summary: []string{"Session.swift:173: cannot call value"}},
		{path: "build.compile/lineage-observable-macro.log", summary: []string{"macro expansion @Observable:1:"}},
		{path: "signing.entitlement_mismatch/screengate-app-group.log", summary: []string{"com.apple.security.application-groups"}},
		{path: "signing.device_unregistered/screengate-build.log", summary: []string{"iPhone"}},
		{path: "signing.no_team/screengate-build.log", summary: []string{`"ScreenGate"`}},
		{path: "asc.app_record/barbican-name-taken.log", summary: []string{"dev.barbican.app"}, human: api.HumanCreateAppRecord},
		{path: "asc.agreement/asc-403-required-agreements.log", human: api.HumanAcceptAgreement},
		{path: "xcode.account/barbican-failed-to-use-accounts.log", human: api.HumanXcodeSignIn},
		{path: "device.locked/bodydiff-launch.log", human: api.HumanUnlockPhone},
		{path: "device.unavailable/devicectl-connection-timeout.log", human: api.HumanUnlockPhone},
		{path: "device.unavailable/devicectl-install-asleep.log", human: api.HumanUnlockPhone},
		{path: "device.unavailable/devicectl-install-ddi-locked.log", human: api.HumanUnlockPhone},
		{path: "upload.export_compliance/asc-missing-compliance.log", human: api.HumanExportCompliance},
		{path: "upload.export_compliance/itms-90592.log", hint: []string{"ITSEncryptionExportComplianceCode"}},
		{path: "signing.cloud_permission/screengate-export.log", hint: []string{"Admin"}},
		{path: "signing.cloud_permission/screengate-export.log", ctx: map[string]string{"signing": "api-key"}, hint: []string{"configured for api-key signing"}},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "logs", c.path))
			if err != nil {
				t.Fatal(err)
			}
			class := filepath.Dir(c.path)
			phase := c.phase
			if phase == "" {
				phase = phaseFor(class)
			}
			v := Diagnose(Input{Phase: phase, Log: b, Context: c.ctx})
			if v.Failure.Class != class {
				t.Fatalf("class %s, want %s", v.Failure.Class, class)
			}
			for _, s := range c.summary {
				if !strings.Contains(v.Failure.Summary, s) {
					t.Errorf("summary %q lacks %q", v.Failure.Summary, s)
				}
			}
			for _, s := range c.hint {
				if !strings.Contains(v.Failure.Hint, s) {
					t.Errorf("hint %q lacks %q", v.Failure.Hint, s)
				}
			}
			if c.human != "" && (v.Human == nil || v.Human.Action != c.human) {
				t.Errorf("human %+v, want %s", v.Human, c.human)
			}
		})
	}
}

func TestExitCodeFallback(t *testing.T) {
	log := []byte("Command line invocation:\n    xcodebuild -destination id=[udid] build\n")
	got := Classify(Input{Phase: api.StateBuilding, Log: log, ExitCode: 70})
	if got.Class != ClassDeviceUnavailable {
		t.Fatalf("exit 70 while building: %s", got.Class)
	}
	if got := Classify(Input{Phase: api.StateTesting, Log: log, ExitCode: 70}); got.Class != ClassUnknown {
		t.Fatalf("exit 70 outside device phases: %s", got.Class)
	}
	if got := Classify(Input{Log: log, ExitCode: 70}); got.Class != ClassUnknown {
		t.Fatalf("exit 70 without a phase: %s", got.Class)
	}
}

func TestPhaseScoping(t *testing.T) {
	log := []byte("✘ Test parses() recorded an issue at ParserTests.swift:12:5: Expectation failed: x == 1\n")
	if got := Classify(Input{Phase: api.StatePreparing, Log: log}).Class; got != ClassTestsPre {
		t.Fatalf("preparing: %s", got)
	}
	for _, p := range []api.State{"", api.StateTesting, api.StateBuilding} {
		if got := Classify(Input{Phase: p, Log: log}).Class; got != ClassTestsFailed {
			t.Fatalf("phase %q: %s", p, got)
		}
	}
}

// TestTraps holds lines from real successful logs that look like failures.
func TestTraps(t *testing.T) {
	traps := []string{
		"✔ Test failedReconnectEndsAsLost() passed after 0.001 seconds.",
		"	 Executed 0 tests, with 0 failures (0 unexpected) in 0.000 (0.001) seconds",
		"2026-10-14 13:56:50.035 xcodebuild[65719:32726574] [MT] IDELaunchParametersSnapshot: debugger version lookup failed for path '<nil>': noURL",
		`AppStoreConnectErrorResponse(errors: Optional([XCServices.AppStoreConnectErrorResponse.Errors(code: "ENTITY_ERROR.ATTRIBUTE.INVALID.DUPLICATE.DIFFERENT_ACCOUNT", detail: "The App Name you entered is already being used."`,
		`"Error Domain=IDEDistributionSymbolsStepError Code=1 \"warning: Upload Symbols Failed. The archive did not include a dSYM for the WebRTC.framework\""`,
		"~/Driver/Views/Map.swift:12:5: warning: 'UIDevice' is unavailable in application extensions",
		"error: 'UIDevice.current' is unavailable",
		"Make sure a bearer token was provided",
		"    /usr/bin/codesign --force --sign [udid] --entitlements ~/x.xcent ~/x.app",
	}
	for _, l := range traps {
		got := Classify(Input{Phase: api.StateBuilding, Log: []byte(l)})
		if got.Class != ClassUnknown {
			t.Errorf("%q classified as %s", l, got.Class)
		}
	}
}

func TestTable(t *testing.T) {
	humans := []string{
		"", api.HumanUnlockPhone, api.HumanXcodeSignIn, api.HumanAcceptAgreement, api.HumanExportCompliance,
		api.HumanCreateAppRecord, api.HumanConsoleLogin, api.HumanAcceptTestflightInvite,
	}
	for _, r := range rules {
		if !slices.Contains(humans, r.human) {
			t.Errorf("%s: unknown human action %q", r.class, r.human)
		}
		if r.summary == "" || r.hint == "" {
			t.Errorf("%s: missing summary or hint", r.class)
		}
	}
	// Every class a log can produce has at least one fixture, and fixture directories are
	// classes (or the two negative directories).
	dirs, err := filepath.Glob(filepath.Join("testdata", "logs", "*"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, d := range dirs {
		have[filepath.Base(d)] = true
	}
	for _, r := range rules {
		if len(r.keys) > 0 && !have[r.class] {
			t.Errorf("%s has no fixture", r.class)
		}
	}
	for d := range have {
		if d != dirSuccess && d != dirUnknown && !slices.Contains(Classes(), d) {
			t.Errorf("fixture directory %s is not a class", d)
		}
	}
	for _, c := range []string{ClassBuildNumberMismatch, ClassASCProcessingDeadline, ClassBundleSecretMaterial, ClassSizeConfirmationTimeout} {
		if f := ForClass(c); f == nil || f.Summary == "" || f.By != "rules" {
			t.Errorf("ForClass(%s) = %+v", c, f)
		}
	}
	if ForClass("nope") != nil {
		t.Error("ForClass of an unknown class")
	}
}

func TestUnknownSummary(t *testing.T) {
	f := Classify(Input{Phase: api.StateArchiving, Log: []byte("something odd\n"), ExitCode: 65})
	if f.Class != ClassUnknown || !strings.Contains(f.Summary, "archiving") || !strings.Contains(f.Summary, "exit 65") {
		t.Fatalf("%+v", f)
	}
	if len(f.Excerpt) != 1 || f.Excerpt[0] != "something odd" {
		t.Fatalf("excerpt %q", f.Excerpt)
	}
	if f := Classify(Input{}); f.Class != ClassUnknown || len(f.Excerpt) != 0 {
		t.Fatalf("empty input: %+v", f)
	}
}

func BenchmarkClassify(b *testing.B) {
	raw, err := os.ReadFile(filepath.Join("testdata", "logs", "success", "driver-archive.log"))
	if err != nil {
		b.Fatal(err)
	}
	var log []byte
	for len(log) < 5<<20 {
		log = append(log, raw...)
	}
	b.SetBytes(int64(len(log)))
	for b.Loop() {
		_ = Classify(Input{Phase: api.StateArchiving, Log: log, ExitCode: 65})
	}
}
