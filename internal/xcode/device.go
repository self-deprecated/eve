package xcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/self-deprecated/eve/internal/run"
)

// DeviceSpec describes a development build for a physical iPhone (docs/design.md §6.4). It builds for
// generic/platform=iOS, so the phone need not be awake, unlocked or even reachable while it
// builds; the development profile already lists every registered device.
type DeviceSpec struct {
	Dir           string // the project's directory
	Project       string // .xcodeproj or .xcworkspace, relative to Dir
	Scheme        string
	Configuration string // Debug unless eve.yml or --release says otherwise
	DerivedData   string
	XCConfig      string
	Auth          *APIKey
}

// DeviceBuildArgs returns xcodebuild's arguments for a device build.
func DeviceBuildArgs(s DeviceSpec) []string {
	args := []string{
		"build", projectFlag(s.Project), s.Project, "-scheme", s.Scheme, "-configuration", s.Configuration,
		"-destination", "generic/platform=iOS",
	}
	if s.DerivedData != "" {
		args = append(args, "-derivedDataPath", s.DerivedData)
	}
	if s.XCConfig != "" {
		args = append(args, "-xcconfig", s.XCConfig)
	}
	args = append(args, "-allowProvisioningUpdates")
	return append(args, s.Auth.args()...)
}

// DeviceBuild runs a device build.
func (t Tools) DeviceBuild(ctx context.Context, s DeviceSpec, log io.Writer) error {
	return t.must(ctx, run.Cmd{Name: "/usr/bin/xcodebuild", Args: DeviceBuildArgs(s), Dir: s.Dir, Env: t.Env, Stdout: log, Stderr: log}, "xcodebuild build")
}

// BuiltApp returns the app a device build produced: the only .app in the products directory, or
// the one whose CFBundleIdentifier is bundleID.
func (t Tools) BuiltApp(ctx context.Context, s DeviceSpec, bundleID string) (string, error) {
	dir := filepath.Join(s.DerivedData, "Build", "Products", s.Configuration+"-iphoneos")
	apps, _ := filepath.Glob(filepath.Join(dir, "*.app"))
	if len(apps) == 1 {
		return apps[0], nil
	}
	for _, a := range apps {
		if id, err := t.plistValue(ctx, filepath.Join(a, "Info.plist"), "CFBundleIdentifier"); err == nil && id == bundleID {
			return a, nil
		}
	}
	return "", fmt.Errorf("found %d apps in %s, none with bundle id %s", len(apps), dir, bundleID)
}

// Device is a device CoreDevice knows about.
type Device struct {
	ID       string // CoreDevice identifier
	UDID     string
	ECID     string // decimal
	Serial   string
	Name     string
	OS       string // e.g. "27.0.1"
	Platform string // iOS, …
	Type     string // iPhone, iPad, …
	Physical bool
	Paired   bool
}

// Identifiers are the values that identify this physical device in logs: the CoreDevice
// identifier, UDID, serial number and ECID (decimal and hex, as devicectl and xcodebuild print it).
func (d Device) Identifiers() []string {
	ids := []string{d.ID, d.UDID, strings.ToLower(d.UDID), d.Serial, d.ECID}
	if n, err := strconv.ParseUint(d.ECID, 10, 64); err == nil {
		ids = append(ids, fmt.Sprintf("%X", n), fmt.Sprintf("%x", n), fmt.Sprintf("%016X", n), fmt.Sprintf("%016x", n))
	}
	return slices.DeleteFunc(ids, func(s string) bool { return s == "" })
}

// Matches reports whether want names d by identifier, UDID or name.
func (d Device) Matches(want string) bool {
	return want == d.ID || strings.EqualFold(want, d.UDID) || want == d.Name
}

// ParseDevices decodes `devicectl list devices --json-output`. It reads the `properties`
// dictionary Xcode 27 introduced and falls back to the fields it deprecated.
func ParseDevices(b []byte) ([]Device, error) {
	var v struct {
		Result struct {
			Devices []struct {
				Identifier string `json:"identifier"`
				Properties *struct {
					Connection struct {
						PairingState string `json:"pairingState"`
					} `json:"connection"`
					Hardware struct {
						UDID       string      `json:"udid"`
						ECID       json.Number `json:"ecid"`
						Serial     string      `json:"serialNumber"`
						Platform   string      `json:"platform"`
						DeviceType string      `json:"deviceType"`
						Reality    string      `json:"reality"`
					} `json:"hardware"`
					Software struct {
						OSVersionNumber struct {
							StringValue string `json:"stringValue"`
						} `json:"osVersionNumber"`
					} `json:"software"`
					State struct {
						Name string `json:"name"`
					} `json:"state"`
				} `json:"properties"`
				Connection struct {
					PairingState string `json:"pairingState"`
				} `json:"connectionProperties"`
				Hardware struct {
					UDID       string      `json:"udid"`
					ECID       json.Number `json:"ecid"`
					Serial     string      `json:"serialNumber"`
					Platform   string      `json:"platform"`
					DeviceType string      `json:"deviceType"`
					Reality    string      `json:"reality"`
				} `json:"hardwareProperties"`
				DeviceProps struct {
					Name      string `json:"name"`
					OSVersion string `json:"osVersionNumber"`
				} `json:"deviceProperties"`
			} `json:"devices"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("devicectl list devices: %w", err)
	}
	out := make([]Device, 0, len(v.Result.Devices))
	for _, x := range v.Result.Devices {
		d := Device{
			ID: x.Identifier, UDID: x.Hardware.UDID, ECID: x.Hardware.ECID.String(), Serial: x.Hardware.Serial,
			Name: x.DeviceProps.Name, OS: x.DeviceProps.OSVersion,
			Platform: x.Hardware.Platform, Type: x.Hardware.DeviceType,
			Physical: x.Hardware.Reality != "simulated", Paired: x.Connection.PairingState == "paired",
		}
		if p := x.Properties; p != nil {
			d.UDID, d.Name, d.OS = first(p.Hardware.UDID, d.UDID), first(p.State.Name, d.Name), first(p.Software.OSVersionNumber.StringValue, d.OS)
			d.ECID, d.Serial = first(p.Hardware.ECID.String(), d.ECID), first(p.Hardware.Serial, d.Serial)
			d.Platform, d.Type = first(p.Hardware.Platform, d.Platform), first(p.Hardware.DeviceType, d.Type)
			if p.Hardware.Reality != "" {
				d.Physical = p.Hardware.Reality != "simulated"
			}
			if p.Connection.PairingState != "" {
				d.Paired = p.Connection.PairingState == "paired"
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Devices lists the devices CoreDevice knows about, Simulators included.
func (t Tools) Devices(ctx context.Context) ([]Device, error) {
	out, err := t.devicectl(ctx, nil, "list", "devices", "--json-output", "-")
	if err != nil {
		return nil, err
	}
	return ParseDevices(out)
}

// Install installs app on device, logging devicectl's output.
func (t Tools) Install(ctx context.Context, device, app string, log io.Writer) error {
	_, err := t.devicectl(ctx, log, "device", "install", "app", "--device", device, app)
	return err
}

// Launch starts bundleID on device, replacing a running copy.
func (t Tools) Launch(ctx context.Context, device, bundleID string, log io.Writer) error {
	_, err := t.devicectl(ctx, log, "device", "process", "launch", "--device", device, "--terminate-existing", bundleID)
	return err
}

// devicectl runs `xcrun devicectl args…`, returning stdout. With log set, both streams go to it
// as well. A non-zero exit is an ExitError, with stdout still returned (devicectl writes its
// JSON error document there).
func (t Tools) devicectl(ctx context.Context, log io.Writer, args ...string) ([]byte, error) {
	var out, errOut bytes.Buffer
	stdout, stderr := io.Writer(&out), io.Writer(&errOut)
	if log != nil {
		stdout, stderr = io.MultiWriter(&out, log), io.MultiWriter(&errOut, log)
	}
	what := "devicectl " + strings.Join(args[:2], " ")
	res, err := t.Runner.Run(ctx, run.Cmd{Name: "/usr/bin/xcrun", Args: append([]string{"devicectl"}, args...), Env: t.Env, Stdout: stdout, Stderr: stderr, OnStart: t.OnStart})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if res.ExitCode != 0 {
		return out.Bytes(), &ExitError{What: what, ExitCode: res.ExitCode}
	}
	return out.Bytes(), nil
}
