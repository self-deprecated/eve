package xcode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	if _, err := os.Stat("/usr/bin/rsync"); err != nil {
		t.Skip("no /usr/bin/rsync on this host")
	}
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
