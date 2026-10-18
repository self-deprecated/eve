// Package xcode drives Apple's command-line tools for the pipeline: the child environment,
// XcodeGen, xcodebuild archive and export, and checks on the archived app (PLAN.md §4.1, §6.2,
// §6.3). Every command goes through run.Runner with an explicit environment; no shell that reads
// startup files is ever involved.
package xcode

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/run"
)

// SystemPath is the PATH every child gets: Apple's tools first, so Nix's rsync can never be
// picked up by Xcode's IPA packaging (M0 spike 1).
const SystemPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// EnvSpec describes the child environment.
type EnvSpec struct {
	Home         string
	User         string
	TmpDir       string
	DeveloperDir string   // from `xcode-select -p` or a pinned Xcode
	ExtraPath    []string // eve's own tool directories, appended after SystemPath
}

// ChildEnv returns the complete, allowlisted environment for build commands.
func ChildEnv(s EnvSpec) []string {
	path := SystemPath
	if len(s.ExtraPath) > 0 {
		path += ":" + strings.Join(s.ExtraPath, ":")
	}
	env := []string{
		"HOME=" + s.Home,
		"USER=" + s.User,
		"LOGNAME=" + s.User,
		"PATH=" + path,
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		"NSUnbufferedIO=YES",
	}
	if s.TmpDir != "" {
		env = append(env, "TMPDIR="+s.TmpDir)
	}
	if s.DeveloperDir != "" {
		env = append(env, "DEVELOPER_DIR="+s.DeveloperDir)
	}
	return env
}

// Tools runs Apple's tools.
type Tools struct {
	Runner run.Runner
	Env    []string
	// OnStart receives each command's process group so the job can record it.
	OnStart func(pgid int)
}

// DeveloperDir returns the selected Xcode's developer directory.
func DeveloperDir(ctx context.Context, r run.Runner) (string, error) {
	var out bytes.Buffer
	res, err := r.Run(ctx, run.Cmd{Name: "/usr/bin/xcode-select", Args: []string{"-p"}, Env: []string{"PATH=" + SystemPath}, Stdout: &out})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("xcode-select -p exited %d", res.ExitCode)
	}
	return strings.TrimSpace(out.String()), nil
}

// Version returns the output of `xcodebuild -version`, e.g. "Xcode 27.0\nBuild version 27A266a".
func (t Tools) Version(ctx context.Context) (string, error) {
	var out bytes.Buffer
	res, err := t.Runner.Run(ctx, run.Cmd{Name: "/usr/bin/xcodebuild", Args: []string{"-version"}, Env: t.Env, Stdout: &out, Stderr: &out})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("xcodebuild -version exited %d: %s", res.ExitCode, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// CheckVersion verifies the selected Xcode matches want ("27.0" matches "Xcode 27.0").
func CheckVersion(versionOutput, want string) error {
	if want == "" {
		return nil
	}
	first, _, _ := strings.Cut(versionOutput, "\n")
	have := strings.TrimSpace(strings.TrimPrefix(first, "Xcode"))
	if have != want && !strings.HasPrefix(have, want+".") {
		return fmt.Errorf("eve.yml pins Xcode %s but the selected Xcode is %s", want, have)
	}
	return nil
}

// AssertSystemRsync fails when the first rsync on env's PATH is not /usr/bin/rsync: Xcode's IPA
// export breaks with Nix's (PLAN.md §4.1). macOS always ships /usr/bin/rsync, so no rsync at all
// only happens off the Mac, where xcodebuild doesn't run either.
func AssertSystemRsync(env []string) error {
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, "rsync")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			if p != "/usr/bin/rsync" {
				return fmt.Errorf("rsync resolves to %s, not /usr/bin/rsync; Xcode's IPA export breaks with it", p)
			}
			return nil
		}
	}
	return nil
}

// EnsureXcodegen returns the path to the pinned XcodeGen, downloading and verifying it into
// toolsDir on first use.
func EnsureXcodegen(ctx context.Context, toolsDir, version, sha string) (string, error) {
	dir := filepath.Join(toolsDir, "xcodegen-"+version)
	bin := filepath.Join(dir, "xcodegen", "bin", "xcodegen")
	if fi, err := os.Stat(bin); err == nil && fi.Mode()&0o111 != 0 {
		return bin, nil
	}
	url := fmt.Sprintf("https://github.com/yonaskolb/XcodeGen/releases/download/%s/xcodegen.zip", version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download XcodeGen %s: %w", version, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download XcodeGen %s: HTTP %d", version, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != sha {
		return "", fmt.Errorf("XcodeGen %s checksum mismatch: got %s, want %s", version, got, sha)
	}
	if err := unzip(body, dir); err != nil {
		return "", err
	}
	return bin, nil
}

func unzip(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".xcodegen-")
	if err != nil {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if tmp, err = os.MkdirTemp(filepath.Dir(dest), ".xcodegen-"); err != nil {
			return err
		}
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for _, f := range zr.File {
		name := filepath.Clean(f.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return fmt.Errorf("unsafe path in XcodeGen archive: %s", f.Name)
		}
		target := filepath.Join(tmp, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if f.Mode()&0o111 != 0 {
			mode = 0o755
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			_ = rc.Close()
			return err
		}
		_, err = io.Copy(out, io.LimitReader(rc, 200<<20))
		_ = rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	_ = os.RemoveAll(dest)
	return os.Rename(tmp, dest)
}

// Generate runs XcodeGen in projectDir.
func (t Tools) Generate(ctx context.Context, xcodegen, projectDir, spec string, log io.Writer) error {
	args := []string{"generate", "--quiet"}
	if spec != "" && spec != "project.yml" {
		args = append(args, "--spec", spec)
	}
	return t.must(ctx, run.Cmd{Name: xcodegen, Args: args, Dir: projectDir, Env: t.Env, Stdout: log, Stderr: log}, "xcodegen generate")
}

// APIKey authenticates xcodebuild with an App Store Connect API key instead of the Xcode account.
type APIKey struct {
	Path, ID, Issuer string
}

func (k *APIKey) args() []string {
	if k == nil {
		return nil
	}
	return []string{"-authenticationKeyPath", k.Path, "-authenticationKeyID", k.ID, "-authenticationKeyIssuerID", k.Issuer}
}

// ArchiveSpec describes an archive build.
type ArchiveSpec struct {
	Dir         string // working directory (the project's directory)
	Project     string // .xcodeproj, relative to Dir
	Scheme      string
	ArchivePath string
	DerivedData string
	BuildNumber string
	XCConfig    string // optional secrets.xcconfig (PLAN.md §6.2)
	Settings    map[string]string
	Auth        *APIKey
}

// ArchiveArgs returns xcodebuild's arguments for spec (exported for tests and dry runs).
func ArchiveArgs(s ArchiveSpec) []string {
	args := []string{
		"archive", "-project", s.Project, "-scheme", s.Scheme, "-configuration", "Release",
		"-destination", "generic/platform=iOS", "-archivePath", s.ArchivePath,
	}
	if s.DerivedData != "" {
		args = append(args, "-derivedDataPath", s.DerivedData)
	}
	if s.XCConfig != "" {
		args = append(args, "-xcconfig", s.XCConfig)
	}
	args = append(args, "-allowProvisioningUpdates")
	args = append(args, s.Auth.args()...)
	if s.BuildNumber != "" {
		args = append(args, "CURRENT_PROJECT_VERSION="+s.BuildNumber)
	}
	args = append(args, settingArgs(s.Settings)...)
	return args
}

// Archive runs xcodebuild archive, writing the full log to log.
func (t Tools) Archive(ctx context.Context, s ArchiveSpec, log io.Writer) error {
	return t.must(ctx, run.Cmd{Name: "/usr/bin/xcodebuild", Args: ArchiveArgs(s), Dir: s.Dir, Env: t.Env, Stdout: log, Stderr: log}, "xcodebuild archive")
}

// ExportSpec is the ExportOptions.plist content eve controls.
type ExportSpec struct {
	Destination   string // "upload" or "export"
	Team          string
	InternalOnly  bool
	UploadSymbols bool
}

// ExportOptionsPlist renders ExportOptions.plist.
func ExportOptionsPlist(s ExportSpec) []byte {
	b := func(v bool) string {
		if v {
			return "<true/>"
		}
		return "<false/>"
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>method</key><string>app-store-connect</string>
  <key>destination</key><string>` + xmlEscape(s.Destination) + `</string>
  <key>teamID</key><string>` + xmlEscape(s.Team) + `</string>
  <key>signingStyle</key><string>automatic</string>
  <key>manageAppVersionAndBuildNumber</key><false/>
  <key>uploadSymbols</key>` + b(s.UploadSymbols) + `
  <key>testFlightInternalTestingOnly</key>` + b(s.InternalOnly) + `
</dict></plist>
`)
}

// ExportArgs returns xcodebuild's -exportArchive arguments.
func ExportArgs(archive, options, exportPath string, auth *APIKey) []string {
	args := []string{"-exportArchive", "-archivePath", archive, "-exportOptionsPlist", options, "-exportPath", exportPath, "-allowProvisioningUpdates"}
	return append(args, auth.args()...)
}

// Export runs xcodebuild -exportArchive. With destination=upload this signs and uploads in one
// call, which is why eve treats it as a single "delivering" phase (PLAN.md §5.1).
func (t Tools) Export(ctx context.Context, archive, options, exportPath string, auth *APIKey, log io.Writer) error {
	if err := AssertSystemRsync(t.Env); err != nil {
		return err
	}
	return t.must(ctx, run.Cmd{Name: "/usr/bin/xcodebuild", Args: ExportArgs(archive, options, exportPath, auth), Env: t.Env, Stdout: log, Stderr: log}, "xcodebuild -exportArchive")
}

// ArchivedApp returns the .app inside an archive.
func ArchivedApp(archive string) (string, error) {
	apps, err := filepath.Glob(filepath.Join(archive, "Products", "Applications", "*.app"))
	if err != nil {
		return "", err
	}
	if len(apps) != 1 {
		return "", fmt.Errorf("expected one app in %s/Products/Applications, found %d", archive, len(apps))
	}
	return apps[0], nil
}

// BundleVersions reads CFBundleVersion and CFBundleShortVersionString from an app and every
// embedded extension (PlugIns/*.appex, Extensions/*.appex), keyed by path relative to the app.
func (t Tools) BundleVersions(ctx context.Context, app string) (map[string][2]string, error) {
	bundles := []string{app}
	for _, pat := range []string{"PlugIns/*.appex", "Extensions/*.appex", "Watch/*.app"} {
		m, _ := filepath.Glob(filepath.Join(app, pat))
		bundles = append(bundles, m...)
	}
	out := map[string][2]string{}
	for _, b := range bundles {
		plist := filepath.Join(b, "Info.plist")
		build, err := t.plistValue(ctx, plist, "CFBundleVersion")
		if err != nil {
			return nil, err
		}
		short, _ := t.plistValue(ctx, plist, "CFBundleShortVersionString")
		rel, _ := filepath.Rel(filepath.Dir(app), b)
		out[rel] = [2]string{build, short}
	}
	return out, nil
}

// CheckBuildNumbers returns the bundles whose CFBundleVersion differs from want.
func CheckBuildNumbers(versions map[string][2]string, want string) []string {
	var bad []string
	for path, v := range versions {
		if v[0] != want {
			bad = append(bad, fmt.Sprintf("%s has CFBundleVersion %q", path, v[0]))
		}
	}
	sort.Strings(bad)
	return bad
}

func (t Tools) plistValue(ctx context.Context, plist, key string) (string, error) {
	var out, errOut bytes.Buffer
	res, err := t.Runner.Run(ctx, run.Cmd{
		Name: "/usr/bin/plutil", Args: []string{"-extract", key, "raw", "-o", "-", plist},
		Env: t.Env, Stdout: &out, Stderr: &errOut,
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("plutil %s %s: %s", key, plist, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// WriteXCConfig writes build settings to an xcconfig file readable only by the user, so secrets
// stay out of `ps` and out of xcodebuild's "Build settings from command line" header.
func WriteXCConfig(path string, settings map[string]string) error {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		if !validSetting(k) {
			return fmt.Errorf("invalid build setting name %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("// Generated by eve for one job; removed with the job's workspace.\n")
	for _, k := range keys {
		v := settings[k]
		if strings.ContainsAny(v, "\n\r") {
			return fmt.Errorf("build setting %s contains a newline", k)
		}
		fmt.Fprintf(&b, "%s = %s\n", k, v)
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func validSetting(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func settingArgs(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// ExitError is returned when a tool ran but failed.
type ExitError struct {
	What     string
	ExitCode int
}

func (e *ExitError) Error() string { return fmt.Sprintf("%s exited %d", e.What, e.ExitCode) }

func (t Tools) must(ctx context.Context, c run.Cmd, what string) error {
	c.OnStart = t.OnStart
	res, err := t.Runner.Run(ctx, c)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if res.ExitCode != 0 {
		return &ExitError{What: what, ExitCode: res.ExitCode}
	}
	return nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
