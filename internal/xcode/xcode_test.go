package xcode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/run"
)

func TestChildEnvIsAllowlisted(t *testing.T) {
	env := ChildEnv(EnvSpec{Home: "/Users/jmo", User: "jmo", DeveloperDir: "/Applications/Xcode.app/Contents/Developer", ExtraPath: []string{"/x/tools"}})
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin:/x/tools", "DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer", "LC_ALL=en_US.UTF-8"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, banned := range []string{"SDKROOT=", "CC=", "LD="} {
		if strings.Contains(joined, banned) {
			t.Errorf("%s must never be set", banned)
		}
	}
}

func TestAssertSystemRsync(t *testing.T) {
	if err := AssertSystemRsync([]string{"PATH=" + SystemPath}); err != nil {
		t.Fatal(err)
	}
	nix := t.TempDir()
	if err := os.WriteFile(filepath.Join(nix, "rsync"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := AssertSystemRsync([]string{"PATH=" + nix + ":" + SystemPath}); err == nil {
		t.Fatal("a non-system rsync ahead of /usr/bin was accepted")
	}
}

func TestArchiveArgs(t *testing.T) {
	args := ArchiveArgs(ArchiveSpec{
		Project: "Driver.xcodeproj", Scheme: "Driver", ArchivePath: "/j/Driver.xcarchive",
		DerivedData: "/d", BuildNumber: "202610161512", XCConfig: "/j/secrets.xcconfig", Settings: map[string]string{"B": "2", "A": "1"},
	})
	line := strings.Join(args, " ")
	for _, want := range []string{
		"archive -project Driver.xcodeproj -scheme Driver -configuration Release",
		"-destination generic/platform=iOS", "-xcconfig /j/secrets.xcconfig", "-allowProvisioningUpdates",
		"CURRENT_PROJECT_VERSION=202610161512 A=1 B=2",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
	if strings.Contains(line, "authenticationKey") {
		t.Error("API key args without an API key")
	}
	withKey := strings.Join(ArchiveArgs(ArchiveSpec{Auth: &APIKey{Path: "/k.p8", ID: "K", Issuer: "I"}}), " ")
	if !strings.Contains(withKey, "-authenticationKeyPath /k.p8 -authenticationKeyID K -authenticationKeyIssuerID I") {
		t.Error(withKey)
	}
}

func TestWorkspacesBuildWithWorkspaceFlag(t *testing.T) {
	for name, args := range map[string][]string{
		"archive":   ArchiveArgs(ArchiveSpec{Project: "App.xcworkspace", Scheme: "App"}),
		"device":    DeviceBuildArgs(DeviceSpec{Project: "App.xcworkspace", Scheme: "App", Configuration: "Debug"}),
		"simulator": SimBuildArgs(SimSpec{Project: "App.xcworkspace", Scheme: "App"}),
		"test":      TestArgs(SimSpec{Project: "App.xcworkspace", Scheme: "App"}),
	} {
		if args[1] != "-workspace" || args[2] != "App.xcworkspace" || slices.Contains(args, "-project") {
			t.Errorf("%s: %v", name, args)
		}
	}
	if args := SimBuildArgs(SimSpec{Project: "App.xcodeproj"}); args[1] != "-project" {
		t.Errorf("project: %v", args)
	}
}

func TestExportOptionsPlist(t *testing.T) {
	p := string(ExportOptionsPlist(ExportSpec{Destination: "upload", Team: "2559B88H6C", InternalOnly: true, UploadSymbols: false}))
	for _, want := range []string{
		"<string>upload</string>", "<string>2559B88H6C</string>",
		"<key>testFlightInternalTestingOnly</key><true/>", "<key>uploadSymbols</key><false/>",
		"<key>manageAppVersionAndBuildNumber</key><false/>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestCheckBuildNumbers(t *testing.T) {
	bad := CheckBuildNumbers(map[string][2]string{
		"Driver.app":                       {"999", "0.1"},
		"Driver.app/PlugIns/Widgets.appex": {"1", "0.1"},
		"Driver.app/PlugIns/Intents.appex": {"999", "0.1"},
	}, "999")
	if len(bad) != 1 || !strings.Contains(bad[0], "Widgets.appex") {
		t.Fatal(bad)
	}
}

func TestWriteXCConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secrets.xcconfig")
	if err := WriteXCConfig(p, map[string]string{"BODYDIFF_COACH_CODE": "s3cret"}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "BODYDIFF_COACH_CODE = s3cret") {
		t.Fatal(string(b))
	}
	if err := WriteXCConfig(p, map[string]string{"BAD NAME": "x"}); err == nil {
		t.Fatal("invalid setting accepted")
	}
	if err := WriteXCConfig(p, map[string]string{"X": "a\nB = injected"}); err == nil {
		t.Fatal("newline injection accepted")
	}
}

func TestCheckVersion(t *testing.T) {
	out := "Xcode 27.0\nBuild version 27A266a"
	if err := CheckVersion(out, "27.0"); err != nil {
		t.Fatal(err)
	}
	if err := CheckVersion(out, "27"); err != nil {
		t.Fatal(err)
	}
	if err := CheckVersion(out, "26.6"); err == nil {
		t.Fatal("mismatch accepted")
	}
}

func TestExportRefusesNixRsyncBeforeRunning(t *testing.T) {
	nix := t.TempDir()
	_ = os.WriteFile(filepath.Join(nix, "rsync"), []byte("#!/bin/sh\n"), 0o755)
	f := &run.Fake{}
	tools := Tools{Runner: f, Env: []string{"PATH=" + nix + ":" + SystemPath}}
	if err := tools.Export(context.Background(), "a", "o", "e", nil, nil); err == nil {
		t.Fatal("export ran with a Nix rsync first on PATH")
	}
	if len(f.Calls) != 0 {
		t.Fatal("xcodebuild must not run")
	}
}

func TestToolsMustReportsExit(t *testing.T) {
	f := &run.Fake{Handler: func(c run.Cmd) (string, int, error) { return "", 65, nil }}
	var pg []int
	tools := Tools{Runner: f, Env: []string{"PATH=" + SystemPath}, OnStart: func(p int) { pg = append(pg, p) }}
	err := tools.Archive(context.Background(), ArchiveSpec{Project: "P.xcodeproj", Scheme: "S", ArchivePath: "/a"}, nil)
	var ee *ExitError
	if err == nil || !asExit(err, &ee) || ee.ExitCode != 65 {
		t.Fatalf("want ExitError 65, got %v", err)
	}
	if !slices.Equal(pg, []int{4242}) {
		t.Fatal("pgid not reported")
	}
}

func asExit(err error, target **ExitError) bool {
	e, ok := err.(*ExitError)
	if ok {
		*target = e
	}
	return ok
}

// Shape of `simctl list -j devices` from Xcode 26.5 (CoreSimulator reports lastUsedAt, set at boot).
const simctlDevices = `{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-26-5":[
 {"dataPath":"/Users/jmo/Library/Developer/CoreSimulator/Devices/17FE73E1-7789-478E-BC9C-286CD675D7A0/data",
  "dataPathSize":2325372928,"udid":"17FE73E1-7789-478E-BC9C-286CD675D7A0","lastUsedAt":"2026-11-07T12:51:27Z",
  "isAvailable":true,"deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro",
  "state":"Shutdown","name":"10 & 2 review"},
 {"udid":"81FC86DE-A74C-4643-A332-EBBEC3EBCB9B","state":"Shutdown","name":"never booted","isAvailable":true},
 {"udid":"2A8D25F8-B2D2-4E62-9CCE-A0A0C764B856","state":"Shutdown","name":"older Xcode",
  "lastUsedAt":"2026-11-01T10:00:00Z","lastBootedAt":"2026-11-02T10:00:00Z"}]}}`

func TestSimulatorInventoryReportsLastBoot(t *testing.T) {
	tools := Tools{Runner: &run.Fake{Handler: func(run.Cmd) (string, int, error) { return simctlDevices, 0, nil }}}
	ds, err := tools.Simulators(context.Background())
	if err != nil || len(ds) != 3 {
		t.Fatal(ds, err)
	}
	want := map[string]string{"10 & 2 review": "2026-11-07T12:51:27Z", "never booted": "0001-01-01T00:00:00Z", "older Xcode": "2026-11-02T10:00:00Z"}
	for _, d := range ds {
		boot, err := d.LastBoot()
		if err != nil || boot.Format(time.RFC3339) != want[d.Name] {
			t.Errorf("%s: %v %v, want %s", d.Name, boot, err, want[d.Name])
		}
	}
	if _, err := (SimDevice{LastUsedAt: "yesterday"}).LastBoot(); err == nil {
		t.Error("unreadable boot time accepted")
	}
}

func TestDeleteSimulatorRefusesNonUDIDs(t *testing.T) {
	fake := &run.Fake{Handler: func(run.Cmd) (string, int, error) { return "", 0, nil }}
	tools := Tools{Runner: fake}
	for _, id := range []string{"", "all", "unavailable", "eve-j_x", "booted"} {
		if err := tools.DeleteSimulator(context.Background(), id); err == nil {
			t.Errorf("deleted %q", id)
		}
	}
	if len(fake.Commands()) != 0 {
		t.Fatal(fake.Commands())
	}
	if err := tools.DeleteSimulator(context.Background(), "81FC86DE-A74C-4643-A332-EBBEC3EBCB9B"); err != nil {
		t.Fatal(err)
	}
	if got := fake.Commands(); len(got) != 1 || !strings.HasSuffix(got[0], "simctl delete 81FC86DE-A74C-4643-A332-EBBEC3EBCB9B") {
		t.Fatal(got)
	}
}
