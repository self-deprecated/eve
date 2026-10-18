package xcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/run"
)

// SimSpec describes a Simulator build or test run (docs/design.md §6.5). Both are Debug builds signed
// ad hoc ("Sign to Run Locally"): no team, account or login keychain is involved, but the app
// keeps its entitlements. Fully unsigned (CODE_SIGNING_ALLOWED=NO) apps lose them, and the
// Simulator's keychain refuses them.
type SimSpec struct {
	Dir          string // the project's directory
	Project      string // .xcodeproj, relative to Dir
	Scheme       string
	DerivedData  string
	XCConfig     string
	SimulatorID  string // test runs only
	ResultBundle string // test runs only
}

// SimBuildArgs returns xcodebuild's arguments for a Simulator build.
func SimBuildArgs(s SimSpec) []string {
	args := []string{
		"build", "-project", s.Project, "-scheme", s.Scheme, "-configuration", "Debug",
		"-destination", "generic/platform=iOS Simulator",
	}
	return append(args, s.common()...)
}

// TestArgs returns xcodebuild's arguments for running the scheme's tests on one Simulator.
func TestArgs(s SimSpec) []string {
	args := []string{
		"test", "-project", s.Project, "-scheme", s.Scheme, "-configuration", "Debug",
		"-destination", "platform=iOS Simulator,id=" + s.SimulatorID, "-destination-timeout", "120",
		"-resultBundlePath", s.ResultBundle, "-parallel-testing-enabled", "NO",
		// On any failure xcodebuild otherwise sysdiagnoses the Simulator for up to ten minutes;
		// the result bundle and the log say what failed.
		"-collect-test-diagnostics", "never",
	}
	return append(args, s.common()...)
}

func (s SimSpec) common() []string {
	var args []string
	if s.DerivedData != "" {
		args = append(args, "-derivedDataPath", s.DerivedData)
	}
	if s.XCConfig != "" {
		args = append(args, "-xcconfig", s.XCConfig)
	}
	return append(args, AdHocSigning...)
}

// AdHocSigning signs Simulator builds locally, overriding the project's team and profiles.
var AdHocSigning = []string{"CODE_SIGN_IDENTITY=-", "CODE_SIGN_STYLE=Manual", "DEVELOPMENT_TEAM=", "PROVISIONING_PROFILE_SPECIFIER="}

// SimulatorBuild runs a Simulator build.
func (t Tools) SimulatorBuild(ctx context.Context, s SimSpec, log io.Writer) error {
	return t.must(ctx, run.Cmd{Name: "/usr/bin/xcodebuild", Args: SimBuildArgs(s), Dir: s.Dir, Env: t.Env, Stdout: log, Stderr: log}, "xcodebuild build")
}

// Test runs the scheme's tests on s.SimulatorID, writing s.ResultBundle.
func (t Tools) Test(ctx context.Context, s SimSpec, log io.Writer) error {
	return t.must(ctx, run.Cmd{Name: "/usr/bin/xcodebuild", Args: TestArgs(s), Dir: s.Dir, Env: t.Env, Stdout: log, Stderr: log}, "xcodebuild test")
}

// SimulatorPrefix names every Simulator eve creates. The worker runs one job at a time, so any
// Simulator with this prefix that exists when a test run starts was orphaned by a crash.
const SimulatorPrefix = "eve-"

// SimDeviceType is a Simulator device type.
type SimDeviceType struct {
	Identifier    string `json:"identifier"`
	Name          string `json:"name"`
	ProductFamily string `json:"productFamily"`
}

// SimRuntime is an installed Simulator runtime.
type SimRuntime struct {
	Identifier  string          `json:"identifier"`
	Platform    string          `json:"platform"`
	Version     string          `json:"version"`
	IsAvailable bool            `json:"isAvailable"`
	DeviceTypes []SimDeviceType `json:"supportedDeviceTypes"`
}

// PickSimulator chooses the newest available iOS runtime and an iPhone it supports: the device
// type named want if given, else the newest "Pro" model (iPhone 17 Pro over iPhone 16 Pro Max).
func PickSimulator(runtimes []SimRuntime, want string) (runtime, deviceType string, err error) {
	var best *SimRuntime
	for i := range runtimes {
		r := &runtimes[i]
		if r.Platform != "iOS" || !r.IsAvailable {
			continue
		}
		if best == nil || versionLess(best.Version, r.Version) {
			best = r
		}
	}
	if best == nil {
		return "", "", fmt.Errorf("no iOS Simulator runtime is installed (Xcode > Settings > Components)")
	}
	pick, score := "", -1
	for _, d := range best.DeviceTypes {
		if d.ProductFamily != "iPhone" {
			continue
		}
		if want != "" {
			if d.Name == want {
				return best.Identifier, d.Identifier, nil
			}
			continue
		}
		if s := iphoneScore(d.Name); s > score {
			pick, score = d.Identifier, s
		}
	}
	if want != "" {
		return "", "", fmt.Errorf("iOS %s has no Simulator device type named %q", best.Version, want)
	}
	if pick == "" {
		return "", "", fmt.Errorf("iOS %s supports no iPhone Simulator", best.Version)
	}
	return best.Identifier, pick, nil
}

var modelNumber = regexp.MustCompile(`^iPhone (\d+)`)

// iphoneScore ranks device types: newest model number first, then plain "Pro", then the rest.
func iphoneScore(name string) int {
	m := modelNumber.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	s := n * 10
	switch strings.TrimPrefix(name, m[0]) {
	case " Pro":
		s += 3
	case "":
		s += 2
	case " Pro Max":
		s++
	}
	return s
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func (t Tools) simctl(ctx context.Context, out io.Writer, args ...string) error {
	var buf bytes.Buffer
	w := io.Writer(&buf)
	if out != nil {
		w = io.MultiWriter(&buf, out)
	}
	res, err := t.Runner.Run(ctx, run.Cmd{Name: "/usr/bin/xcrun", Args: append([]string{"simctl"}, args...), Env: t.Env, Stdout: w, Stderr: &buf, OnStart: t.OnStart})
	if err != nil {
		return fmt.Errorf("simctl %s: %w", args[0], err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("simctl %s exited %d: %s", args[0], res.ExitCode, strings.TrimSpace(lastLine(buf.String())))
	}
	return nil
}

// Runtimes lists the installed Simulator runtimes.
func (t Tools) Runtimes(ctx context.Context) ([]SimRuntime, error) {
	var out bytes.Buffer
	if err := t.simctl(ctx, &out, "list", "-j", "runtimes", "available"); err != nil {
		return nil, err
	}
	var v struct {
		Runtimes []SimRuntime `json:"runtimes"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		return nil, fmt.Errorf("simctl list runtimes: %w", err)
	}
	return v.Runtimes, nil
}

// CreateSimulator creates and boots a fresh Simulator named name and returns its UDID.
func (t Tools) CreateSimulator(ctx context.Context, name, deviceType, runtime string) (string, error) {
	var out bytes.Buffer
	if err := t.simctl(ctx, &out, "create", name, deviceType, runtime); err != nil {
		return "", err
	}
	id := strings.TrimSpace(out.String())
	if !udid.MatchString(id) {
		return "", fmt.Errorf("simctl create returned %q, not a Simulator identifier", id)
	}
	boot, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	if err := t.simctl(boot, nil, "boot", id); err != nil {
		return id, err
	}
	if err := t.simctl(boot, nil, "bootstatus", id, "-b"); err != nil {
		return id, err
	}
	return id, nil
}

var udid = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// DeleteSimulator shuts down and deletes a Simulator. It ignores ctx's cancellation so a
// cancelled job still cleans up.
// A Simulator stuck booting can take minutes to shut down, so the delete gets its own deadline.
func (t Tools) DeleteSimulator(ctx context.Context, id string) error {
	base := context.WithoutCancel(ctx)
	shut, cancel := context.WithTimeout(base, 3*time.Minute)
	_ = t.simctl(shut, nil, "shutdown", id)
	cancel()
	del, cancel := context.WithTimeout(base, time.Minute)
	defer cancel()
	return t.simctl(del, nil, "delete", id)
}

// DeleteOrphans deletes every Simulator whose name starts with prefix and returns how many.
func (t Tools) DeleteOrphans(ctx context.Context, prefix string) (int, error) {
	var out bytes.Buffer
	if err := t.simctl(ctx, &out, "list", "-j", "devices"); err != nil {
		return 0, err
	}
	var v struct {
		Devices map[string][]struct {
			UDID string `json:"udid"`
			Name string `json:"name"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		return 0, fmt.Errorf("simctl list devices: %w", err)
	}
	n := 0
	for _, ds := range v.Devices {
		for _, d := range ds {
			if strings.HasPrefix(d.Name, prefix) && t.DeleteSimulator(ctx, d.UDID) == nil {
				n++
			}
		}
	}
	return n, nil
}

// TestSummary is the part of `xcresulttool get test-results summary` eve reports.
type TestSummary struct {
	Result           string `json:"result"` // Passed, Failed, Skipped, Expected Failure, unknown
	TotalTestCount   int    `json:"totalTestCount"`
	PassedTests      int    `json:"passedTests"`
	FailedTests      int    `json:"failedTests"`
	SkippedTests     int    `json:"skippedTests"`
	ExpectedFailures int    `json:"expectedFailures"`
	TestFailures     []struct {
		TestName       string `json:"testName"`
		TargetName     string `json:"targetName"`
		FailureText    string `json:"failureText"`
		TestIdentifier string `json:"testIdentifierString"`
	} `json:"testFailures"`
}

// ParseTestSummary decodes xcresulttool's summary JSON.
func ParseTestSummary(b []byte) (TestSummary, error) {
	var s TestSummary
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("xcresulttool summary: %w", err)
	}
	return s, nil
}

// ReadTestSummary summarizes a result bundle.
func (t Tools) ReadTestSummary(ctx context.Context, bundle string) (TestSummary, error) {
	var out, errOut bytes.Buffer
	res, err := t.Runner.Run(ctx, run.Cmd{
		Name: "/usr/bin/xcrun", Args: []string{"xcresulttool", "get", "test-results", "summary", "--path", bundle},
		Env: t.Env, Stdout: &out, Stderr: &errOut, OnStart: t.OnStart,
	})
	if err != nil {
		return TestSummary{}, err
	}
	if res.ExitCode != 0 {
		return TestSummary{}, fmt.Errorf("xcresulttool exited %d: %s", res.ExitCode, strings.TrimSpace(lastLine(errOut.String())))
	}
	return ParseTestSummary(out.Bytes())
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
