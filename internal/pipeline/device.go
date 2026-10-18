package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/self-deprecated/eve/internal/xcode"
)

// Device builds a development-signed app, installs it on the user's iPhone and launches it
// (docs/design.md §6.4). The build comes first, so any wait for the person happens after the slow part.
type Device struct{ *Deps }

// NewDevice returns the device pipeline.
func NewDevice(d *Deps) *Device {
	d.defaults()
	return &Device{d}
}

// devicePhases are the phases after the build; a restart in one of them resumes at install.
var devicePhases = []api.State{api.StateInstalling, api.StateWaitingForDevice, api.StateLaunching}

// Run builds (unless a restart found the build already done), then installs and launches.
func (p *Device) Run(jc *queue.JobCtx) error {
	resume := jc.Job.ResumeFrom
	if err := jc.EnterWith(api.StatePreparing, "preparing workspace", recordSource); err != nil {
		return err
	}
	ws, err := p.prepare(jc)
	if err != nil {
		return swallow(err)
	}
	dev, err := p.pick(jc, ws)
	if err != nil {
		return swallow(err)
	}
	kept := filepath.Join(jc.Dir, "device")
	app := keptApp(kept)
	if app == "" || !slices.Contains(devicePhases, resume) {
		if app, err = p.build(jc, ws, kept); err != nil {
			return swallow(err)
		}
	}
	return swallow(p.installAndLaunch(jc, ws, dev, app))
}

// Watch is never called: device jobs wait on the worker, for at most DeviceWait.
func (p *Device) Watch(*queue.JobCtx) error { return nil }

// pick chooses the phone: --device, the app's default, the server's, or the first paired
// physical iPhone.
func (p *Device) pick(jc *queue.JobCtx, ws *workspace) (xcode.Device, error) {
	want := firstOf(jc.Job.Request.Options.Device, ws.app.Device, p.Config.Device.Default)
	devices, err := ws.tools.Devices(jc.Ctx)
	if err != nil {
		if errors.Is(jc.Ctx.Err(), context.Canceled) {
			return xcode.Device{}, jc.Ctx.Err()
		}
		_ = jc.Fail("device.unavailable", "listing devices: "+err.Error(), "Check CoreDevice on the build Mac (`xcrun devicectl list devices`).", nil, api.StateUnavailable)
		return xcode.Device{}, errHandled
	}
	var phones []string
	for _, d := range devices {
		if !d.Physical || d.Platform != "iOS" {
			continue
		}
		if want != "" && d.Matches(want) || want == "" && d.Paired && d.Type == "iPhone" {
			return d, nil
		}
		phones = append(phones, d.Name)
	}
	summary := "no paired iPhone is known to the build Mac"
	if want != "" {
		summary = fmt.Sprintf("no paired device matches %q", want)
	}
	if len(phones) > 0 {
		summary += "; physical devices: " + strings.Join(phones, ", ")
	}
	_ = jc.Fail("device.unknown", summary, "Pair the phone with the build Mac in Xcode (Window > Devices and Simulators), or pass --device with its name or UDID.", nil)
	return xcode.Device{}, errHandled
}

func (p *Device) build(jc *queue.JobCtx, ws *workspace, kept string) (string, error) {
	configuration := ws.repo.Device.Configuration
	if jc.Job.Request.Options.Release {
		configuration = "Release"
	}
	var auth *xcode.APIKey
	if ws.app.SigningMode() == config.SigningAPIKey {
		auth = &xcode.APIKey{Path: p.Config.ASC.KeyPath, ID: p.Config.ASC.KeyID, Issuer: p.Config.ASC.IssuerID}
	}
	spec := xcode.DeviceSpec{
		Dir: ws.projectDir, Project: ws.project, Scheme: ws.repo.Project.Scheme, Configuration: configuration,
		DerivedData: filepath.Join(p.Config.DataDir, "derived", jc.Job.App, "device"), XCConfig: ws.xcconfig, Auth: auth,
	}
	if err := jc.Enter(api.StateBuilding, "building "+configuration+" for the phone"); err != nil {
		return "", err
	}
	if err := p.logged(ws, "build", api.StateBuilding, func(log io.Writer) error {
		return ws.tools.DeviceBuild(jc.Ctx, spec, log)
	}); err != nil {
		return "", err
	}
	built, err := ws.tools.BuiltApp(jc.Ctx, spec, ws.app.BundleID)
	if err != nil {
		_ = jc.Fail("build.no_app", err.Error(), "Check the scheme builds the app target with bundle id "+ws.app.BundleID+".", nil)
		return "", errHandled
	}
	// Keep the job's own copy: the next build of this app reuses the derived data.
	_ = os.RemoveAll(kept)
	if err := os.MkdirAll(kept, 0o700); err != nil {
		return "", err
	}
	app := filepath.Join(kept, filepath.Base(built))
	if err := p.Sources.Clone(built, app); err != nil {
		return "", fmt.Errorf("keeping the built app: %w", err)
	}
	return app, nil
}

// installAndLaunch installs and launches straight away, like a person at Xcode would. A launch
// refused because the phone is locked still succeeds (installed, not launched). Only when the
// phone can't take the install (unreachable, or locked before Xcode's developer disk image is
// mounted) or can't be reached for the launch does the job wait: it enters waiting_for_device
// once and retries that step every DevicePoll, for at most DeviceWait.
func (p *Device) installAndLaunch(jc *queue.JobCtx, ws *workspace, dev xcode.Device, app string) error {
	// The phone's identifiers are personal; keep them out of the install and launch logs.
	ws.secrets = append(ws.secrets, dev.Identifiers()...)
	var deadline time.Time
	steps := []struct {
		state api.State
		msg   string
		run   func(io.Writer) error
	}{
		{api.StateInstalling, "installing on " + dev.Name, func(log io.Writer) error { return ws.tools.Install(jc.Ctx, dev.ID, app, log) }},
		{api.StateLaunching, "launching on " + dev.Name, func(log io.Writer) error {
			return ws.tools.Launch(jc.Ctx, dev.ID, ws.app.BundleID, log)
		}},
	}
	for _, s := range steps {
		if err := jc.Enter(s.state, s.msg); err != nil {
			return err
		}
		refused := false
		for {
			// The phase log keeps every attempt; only this attempt's output is classified, so an
			// earlier "unreachable" never keeps a real failure waiting.
			var attempt bytes.Buffer
			redacted := p.RedactLog(&attempt, ws.secrets)
			runErr := p.writeLog(ws, jc.LogPath(string(s.state)), func(log io.Writer) error {
				return s.run(io.MultiWriter(log, redacted))
			})
			_ = redacted.Close()
			if runErr == nil {
				break
			}
			if errors.Is(jc.Ctx.Err(), context.Canceled) {
				return jc.Ctx.Err()
			}
			f, h := p.classify(ws, s.state, attempt.Bytes(), runErr)
			if s.state == api.StateLaunching && f != nil && f.Class == "device.locked" {
				// Installed, and only the launch needs an unlocked phone: done, as with a person
				// at Xcode. Waiting would make every locked phone a ten-minute job.
				return jc.Succeed("installed on "+dev.Name+"; not launched because the phone is locked", func(j *store.Job) {
					j.Result.Notes = append(j.Result.Notes, api.Note{
						Kind: "not_launched", Detail: dev.Name + " is locked, so the app is installed but not launched; open it on the phone",
					})
				})
			}
			if h == nil || h.Action != api.HumanUnlockPhone && h.Action != api.HumanConnectPhone {
				p.failFrom(jc, string(s.state), s.state, attempt.Bytes(), runErr, ws.app)
				return errHandled
			}
			// why says what's wrong and what to do: "The build Mac can't reach the phone: … Turn on its Wi-Fi …".
			why := "the phone refused the " + string(s.state)
			if f != nil {
				why = strings.TrimSpace(f.Summary + " " + f.Hint)
			}
			switch {
			case deadline.IsZero():
				// The first wait: entering the state notifies the human webhook, once per job.
				deadline = p.Now().Add(p.DeviceWait)
				if err := jc.Enter(api.StateWaitingForDevice, fmt.Sprintf("waiting for %s: %s", dev.Name, why)); err != nil {
					return err
				}
			case !refused:
				jc.Note(why + " Waiting again.")
			}
			refused = true
			if p.Now().After(deadline) {
				_ = jc.NeedsHuman(h.Action, fmt.Sprintf("%s: %s eve gave up after %s; the build was fine, so resubmit once it's sorted.", dev.Name, why, p.DeviceWait),
					"", func(j *store.Job) { j.Result.Failure = f })
				return errHandled
			}
			if err := p.Sleep(jc.Ctx, p.DevicePoll); err != nil {
				return err
			}
		}
	}
	detail := dev.Name
	if dev.OS != "" {
		detail += ", iOS " + dev.OS
	}
	return jc.Succeed("installed and launched on "+dev.Name, func(j *store.Job) {
		j.Result.Notes = append(j.Result.Notes, api.Note{Kind: "launched", Detail: detail})
	})
}

// classify classifies one attempt's output without ending the job.
func (p *Device) classify(ws *workspace, state api.State, b []byte, runErr error) (*api.Failure, *api.Human) {
	if p.Classifier == nil {
		return nil, nil
	}
	exit := -1
	var ee *xcode.ExitError
	if errors.As(runErr, &ee) {
		exit = ee.ExitCode
	}
	return p.Classifier.Classify(state, b, exit, map[string]string{"signing": ws.app.SigningMode(), "phase": string(state)})
}

// keptApp returns the .app a previous run of this job kept, or "".
func keptApp(dir string) string {
	m, _ := filepath.Glob(filepath.Join(dir, "*.app"))
	if len(m) == 1 && fileExists(filepath.Join(m[0], "Info.plist")) {
		return m[0]
	}
	return ""
}

// firstOf returns the first non-empty string.
func firstOf(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
