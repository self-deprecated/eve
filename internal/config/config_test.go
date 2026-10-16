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
	if s.Listen != "127.0.0.1:4790" || !strings.HasSuffix(s.ASC.KeyPath, "/k.p8") || strings.HasPrefix(s.ASC.KeyPath, "~") {
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
