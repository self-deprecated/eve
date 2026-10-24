package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadServerDefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "server.yml", `
team: 2559B88H6C
asc: {keyId: K, issuerId: I, keyPath: ~/k.p8}
sizeGuard:
  bundle: {growth: 25%}
apps:
  lineage:
    bundleId: ai.selfdeprecated.lineage
    export: {uploadSymbols: false}
    sizeGuard:
      bundle: {ceiling: 1GB, allowPatterns: ["*.md"]}
  driver:
    bundleId: ai.selfdeprecated.driver
`)
	s, err := LoadServer(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Listen != "127.0.0.1:4790" || filepath.Base(s.ASC.KeyPath) != "k.p8" || strings.HasPrefix(s.ASC.KeyPath, "~") {
		t.Fatalf("defaults not applied: %+v", s)
	}
	l := s.SizeGuardFor("lineage")
	if l.Bundle.Growth != 25 || l.Bundle.Ceiling != GB || l.Bundle.GrowthMin != 5*MB || len(l.Bundle.AllowPatterns) != 1 {
		t.Fatalf("lineage thresholds: %+v", l.Bundle)
	}
	if time.Duration(l.ConfirmTimeout) != 24*time.Hour {
		t.Fatalf("confirm timeout %v", l.ConfirmTimeout)
	}
	if d := s.SizeGuardFor("driver"); d.Bundle.Ceiling != 300*MB {
		t.Fatalf("driver ceiling %v", d.Bundle.Ceiling)
	}
	if !s.Apps["lineage"].InternalOnly() || s.Apps["lineage"].UploadSymbols() {
		t.Fatal("export defaults wrong")
	}
	if s.Apps["driver"].SigningMode() != SigningXcodeAccount {
		t.Fatal("signing default")
	}
}

func TestServerValidation(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "server.yml", `
listen: 0.0.0.0:4790
apps:
  Bad_Name: {signing: manual, secrets: {X: plain}}
`)
	_, err := LoadServer(p)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"team is required", "loopback", "lowercase", "bundleId", "manual signing", "file:<path>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	p := writeFile(t, t.TempDir(), "server.yml", "team: T\napps: {}\nbogus: 1\n")
	if _, err := LoadServer(p); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestLoadRepo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "eve.yml", "app: driver\nproject: {dir: ios, xcodegen: project.yml, scheme: Driver}\n")
	r, err := LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Device.Configuration != "Debug" || r.Project.Dir != "ios" {
		t.Fatalf("%+v", r)
	}
	sub := filepath.Join(dir, "ios", "Driver")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := FindRepoRoot(sub)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(dir); root != dir && root != want {
		t.Fatalf("root %s", root)
	}
	writeFile(t, dir, "eve.yml", "app: x\nproject: {scheme: S}\n")
	if _, err := LoadRepo(dir); err == nil {
		t.Fatal("missing project kind accepted")
	}
	writeFile(t, dir, "eve.yml", "app: x\nproject: {dir: apps/mobile/ios, xcworkspace: X.xcworkspace, scheme: X}\n")
	if r, err := LoadRepo(dir); err != nil || r.Project.Xcworkspace != "X.xcworkspace" {
		t.Fatalf("workspace: %+v %v", r, err)
	}
	writeFile(t, dir, "eve.yml", "app: x\nproject: {xcodeproj: X.xcodeproj, xcworkspace: X.xcworkspace, scheme: X}\n")
	if _, err := LoadRepo(dir); err == nil {
		t.Fatal("two project kinds accepted")
	}
}

func TestServerToolPath(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadServer(writeFile(t, dir, "server.yml", "team: T\npath: [/nix/store/n-nodejs/bin, /opt/homebrew/bin]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Path) != 2 || s.Path[0] != "/nix/store/n-nodejs/bin" {
		t.Fatalf("path: %v", s.Path)
	}
	for _, bad := range []string{"[bin]", "['/a:/b']"} {
		if _, err := LoadServer(writeFile(t, dir, "server.yml", "team: T\npath: "+bad+"\n")); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("path %s: %v", bad, err)
		}
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]Bytes{"20MB": 20 * MB, "1.5 GB": 1500 * MB, "512": 512, "5mb": 5 * MB} {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v %v", in, got, err)
		}
	}
	if _, err := ParseBytes("lots"); err == nil {
		t.Error("accepted garbage")
	}
}

func TestResolveSecret(t *testing.T) {
	p := writeFile(t, t.TempDir(), "s", "  hunter2\n")
	if v, err := ResolveSecret("file:" + p); err != nil || v != "hunter2" {
		t.Fatalf("%q %v", v, err)
	}
	t.Setenv("EVE_T", "x")
	if v, _ := ResolveSecret("env:EVE_T"); v != "x" {
		t.Fatal(v)
	}
	if _, err := ResolveSecret("plain"); err == nil {
		t.Fatal("plain accepted")
	}
}

func TestEveServerOverridesWithoutClientFile(t *testing.T) {
	t.Setenv("EVE_CONFIG_DIR", t.TempDir())
	t.Setenv("EVE_SERVER", "http://127.0.0.1:4791")
	c, err := LoadClient()
	if err != nil || c.Server != "http://127.0.0.1:4791" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestServerAllowExecAppliesToEveryApp(t *testing.T) {
	p := writeFile(t, t.TempDir(), "server.yml", `
team: T
allowExec: true
apps:
  lineage: {bundleId: ai.selfdeprecated.lineage}
  driver: {bundleId: ai.selfdeprecated.driver, allowExec: true}
`)
	s, err := LoadServer(p)
	if err != nil {
		t.Fatal(err)
	}
	for name, a := range s.Apps {
		if !a.AllowExec {
			t.Fatalf("%s: allowExec not applied", name)
		}
	}
}

func TestSimulatorLifecycleConfiguration(t *testing.T) {
	for _, tc := range []struct {
		body           string
		idle, shutdown time.Duration
		invalid        bool
	}{
		{"", 15 * time.Minute, 30 * time.Second, false},
		{"simulators: {idleTimeout: 0s, shutdownTimeout: 5s}\n", 0, 5 * time.Second, false},
		{"simulators: {idleTimeout: 45m, protectedDevices: [2A8D25F8-B2D2-4E62-9CCE-A0A0C764B856]}\n", 45 * time.Minute, 30 * time.Second, false},
		{"simulators: {idleTimeout: -1m}\n", 0, 0, true},
		{"simulators: {shutdownTimeout: -1s}\n", 0, 0, true},
		{"simulators: {shutdownTimeout: 1ms}\n", 0, 0, true},
		{"simulators: {shutdownTimeout: 3m}\n", 0, 0, true},
		{"simulators: {protectedDevices: [booted]}\n", 0, 0, true},
		{"simulators: {idleTimeout: yesterday}\n", 0, 0, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			p := writeFile(t, t.TempDir(), "server.yml", "team: T\napps: {}\n"+tc.body)
			s, err := LoadServer(p)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid config accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.Simulators.IdleAfter() != tc.idle || s.Simulators.ShutdownWithin() != tc.shutdown {
				t.Fatal(s.Simulators)
			}
		})
	}
}

func TestSimulatorBorrowLeaseLimit(t *testing.T) {
	for _, tc := range []struct {
		setting string
		want    time.Duration
		invalid bool
	}{
		{"", time.Hour, false},
		{"maxLeaseDuration: 0s", time.Hour, false},
		{"maxLeaseDuration: 2h", 2 * time.Hour, false},
		{"maxLeaseDuration: 1s", time.Second, false},
		{"maxLeaseDuration: -1m", 0, true},
		{"maxLeaseDuration: 1ms", 0, true},
		{"maxLeaseDuration: forever", 0, true},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			p := writeFile(t, t.TempDir(), "server.yml", "team: T\napps: {}\nsimulators: {"+tc.setting+"}\n")
			s, err := LoadServer(p)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid cap accepted")
				}
				return
			}
			if err != nil || s.Simulators.LeaseLimit() != tc.want {
				t.Fatal(s, err)
			}
		})
	}
}
