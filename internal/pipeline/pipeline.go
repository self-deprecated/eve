// Package pipeline implements eve's verbs on top of the queue: preparing a workspace, building
// with Apple's tools, the size guard, delivery to App Store Connect and the watchers that follow
// a build through processing (docs/design.md §6).
package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/asc"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/self-deprecated/eve/internal/xcode"
)

// ASC is the part of the App Store Connect client the pipeline uses.
type ASC interface {
	AppByBundleID(ctx context.Context, bundleID string) (asc.App, error)
	LatestBuildNumber(ctx context.Context, appID, version string) (string, bool, error)
	FindBuild(ctx context.Context, appID, version, number string) (*asc.Build, error)
	ReconcileUntil(ctx context.Context, appID, version, number string, window, interval time.Duration) (asc.Reconciliation, error)
	BetaGroupByName(ctx context.Context, appID, name string) (asc.BetaGroup, error)
	AddBuildToGroup(ctx context.Context, groupID, buildID string) error
	SetWhatToTest(ctx context.Context, buildID, text string) (string, error)
	TesterInviteState(ctx context.Context, appID, email string) (asc.TesterInvite, error)
}

// Sources materializes a job's source.
type Sources interface {
	// Extract unpacks the upload with digest sha into dest (hostile-input safe).
	Extract(sha, dest string) error
	// Clone copies a directory tree (copy-on-write where possible).
	Clone(src, dst string) error
}

// SizeGuard measures app bundles and applies the bundle rules (docs/design.md §6.7.2).
type SizeGuard interface {
	// Measure writes the manifest of appDir to manifestPath.
	Measure(appDir, manifestPath string) error
	// Check compares a manifest with an optional baseline manifest.
	Check(manifestPath, baselinePath, baselineBuild string, th config.BundleThresholds, expectChange bool, detailsHint string) (SizeVerdict, error)
}

// SizeVerdict is the outcome of a bundle check.
type SizeVerdict struct {
	Report api.SizeReport
	Secret bool // rule 1: cannot be confirmed
	Trip   bool // rules 2–4: needs the agent's confirmation
}

// Classifier turns a failed phase's log into a failure, and says when a person must act.
type Classifier interface {
	Classify(phase api.State, log []byte, exitCode int, facts map[string]string) (*api.Failure, *api.Human)
	IsDuplicateUpload(log []byte) bool
}

// Deps are the pipeline's collaborators.
type Deps struct {
	Config     *config.Server
	Store      *store.Store
	Runner     run.Runner
	ASC        ASC // nil disables App Store Connect steps (tests, check/device)
	Sources    Sources
	Size       SizeGuard
	Classifier Classifier
	// RedactLog wraps a log file writer so secrets never reach disk (docs/design.md §6.2).
	RedactLog func(w io.Writer, secrets []string) io.WriteCloser
	Now       func() time.Time
	Sleep     func(ctx context.Context, d time.Duration) error

	ReconcileWindow    time.Duration // default 10 min
	ReconcileInterval  time.Duration // default 30 s
	ProcessingDeadline time.Duration // default 4 h
	DeviceWait         time.Duration // how long device waits for the phone, default 10 min
	DevicePoll         time.Duration // how often it checks, default 5 s
	ExecTimeout        time.Duration // longest an exec command may run, default 1 h
}

func (d *Deps) defaults() {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = func(ctx context.Context, dur time.Duration) error {
			t := time.NewTimer(dur)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if d.ReconcileWindow == 0 {
		d.ReconcileWindow = 10 * time.Minute
	}
	if d.ReconcileInterval == 0 {
		d.ReconcileInterval = 30 * time.Second
	}
	if d.ProcessingDeadline == 0 {
		d.ProcessingDeadline = 4 * time.Hour
	}
	if d.DeviceWait == 0 {
		d.DeviceWait = 10 * time.Minute
	}
	if d.DevicePoll == 0 {
		d.DevicePoll = 5 * time.Second
	}
	if d.ExecTimeout == 0 {
		d.ExecTimeout = time.Hour
	}
	if d.RedactLog == nil {
		d.RedactLog = func(w io.Writer, _ []string) io.WriteCloser { return nopCloser{w} }
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// workspace is a prepared job: its source, configuration and environment.
type workspace struct {
	jc         *queue.JobCtx
	src        string // the job's copy of the repository
	repo       *config.Repo
	app        config.App
	tools      xcode.Tools
	env        []string
	projectDir string
	project    string // .xcodeproj or .xcworkspace, relative to projectDir
	secrets    []string
	xcconfig   string
}

// prepare materializes the source, loads eve.yml, builds the child environment, writes secrets,
// generates the project and runs pre (docs/design.md §6.1–§6.2).
func (d *Deps) prepare(jc *queue.JobCtx) (*workspace, error) {
	j := jc.Job
	app, ok := d.Config.Apps[j.App]
	if !ok {
		return nil, d.configErr(jc, fmt.Sprintf("app %s is no longer configured on the server", j.App))
	}
	ws := &workspace{jc: jc, src: filepath.Join(jc.Dir, "src"), app: app}
	if err := os.MkdirAll(jc.Dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(ws.src); err != nil {
		if err := d.materialize(jc, ws.src); err != nil {
			return nil, err
		}
	}
	repo, err := config.LoadRepo(ws.src)
	if err != nil {
		return nil, d.configErr(jc, err.Error())
	}
	if repo.App != j.App {
		return nil, d.configErr(jc, fmt.Sprintf("eve.yml says app %q but the job is for %q", repo.App, j.App))
	}
	ws.repo = repo
	ws.projectDir = filepath.Join(ws.src, filepath.FromSlash(repo.Project.Dir))

	u, _ := user.Current()
	home, _ := os.UserHomeDir()
	devDir, err := xcode.DeveloperDir(jc.Ctx, d.Runner)
	if err != nil {
		return nil, d.configErr(jc, "no Xcode selected: "+err.Error())
	}
	tmp := filepath.Join(jc.Dir, "tmp")
	_ = os.MkdirAll(tmp, 0o700)
	name := ""
	if u != nil {
		name = u.Username
	}
	ws.env = xcode.ChildEnv(xcode.EnvSpec{Home: home, User: name, TmpDir: tmp, DeveloperDir: devDir, ExtraPath: d.Config.Path})
	ws.tools = xcode.Tools{Runner: d.Runner, Env: ws.env, OnStart: jc.OnStart}
	if repo.Xcode != "" {
		v, err := ws.tools.Version(jc.Ctx)
		if err != nil {
			return nil, d.configErr(jc, err.Error())
		}
		if err := xcode.CheckVersion(v, repo.Xcode); err != nil {
			return nil, d.configErr(jc, err.Error())
		}
	}

	if len(app.Secrets) > 0 {
		settings := map[string]string{}
		for k, ref := range app.Secrets {
			v, err := config.ResolveSecret(ref)
			if err != nil {
				return nil, d.configErr(jc, fmt.Sprintf("secret %s: %v", k, err))
			}
			settings[k] = v
			ws.secrets = append(ws.secrets, v)
		}
		ws.xcconfig = filepath.Join(jc.Dir, "secrets.xcconfig")
		if err := xcode.WriteXCConfig(ws.xcconfig, settings); err != nil {
			return nil, d.configErr(jc, err.Error())
		}
	}

	if repo.Project.Xcodegen != "" {
		bin, err := xcode.EnsureXcodegen(jc.Ctx, filepath.Join(d.Config.DataDir, "tools"), d.Config.Xcode.XcodegenVersion, d.Config.Xcode.XcodegenSHA256)
		if err != nil {
			return nil, d.configErr(jc, err.Error())
		}
		if err := d.logged(ws, "generate", api.StatePreparing, func(log io.Writer) error {
			return ws.tools.Generate(jc.Ctx, bin, ws.projectDir, repo.Project.Xcodegen, log)
		}); err != nil {
			return nil, err
		}
	}
	for i, cmd := range repo.Pre {
		phase := fmt.Sprintf("pre-%d", i+1)
		if err := d.logged(ws, phase, api.StatePreparing, func(log io.Writer) error {
			_, _ = fmt.Fprintf(log, "$ %s\n", cmd)
			res, err := d.Runner.Run(jc.Ctx, run.Cmd{
				Name: "/bin/zsh", Args: []string{"-f", "-c", cmd}, Dir: ws.src, Env: ws.env,
				Stdout: log, Stderr: log, OnStart: jc.OnStart,
			})
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return &xcode.ExitError{What: "pre command " + cmd, ExitCode: res.ExitCode}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	// After pre, which may be what generates it (expo prebuild, pod install).
	proj, err := findProject(ws.projectDir, repo)
	if err != nil {
		return nil, d.configErr(jc, err.Error())
	}
	ws.project = proj
	return ws, nil
}

func (d *Deps) materialize(jc *queue.JobCtx, dst string) error {
	req := jc.Job.Request.Source
	switch {
	case req.SHA256 != "":
		unpacked := filepath.Join(jc.Dir, "unpacked")
		if _, err := os.Stat(unpacked); err != nil {
			tmp := unpacked + ".tmp"
			_ = os.RemoveAll(tmp)
			if err := d.Sources.Extract(req.SHA256, tmp); err != nil {
				return d.configErr(jc, "unpacking the source: "+err.Error())
			}
			if err := os.Rename(tmp, unpacked); err != nil {
				return err
			}
		}
		return d.Sources.Clone(unpacked, dst)
	case req.Path != "":
		return d.Sources.Clone(req.Path, dst)
	}
	return d.configErr(jc, "the job has no source")
}

func findProject(dir string, repo *config.Repo) (string, error) {
	if p := firstOf(repo.Project.Xcworkspace, repo.Project.Xcodeproj); p != "" {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			return "", fmt.Errorf("%s not found in %s", p, repo.Project.Dir)
		}
		return p, nil
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*.xcodeproj"))
	if len(m) != 1 {
		return "", fmt.Errorf("expected one .xcodeproj in %s after XcodeGen, found %d", repo.Project.Dir, len(m))
	}
	return filepath.Base(m[0]), nil
}

// errHandled marks an error whose job has already been ended with a classified failure.
var errHandled = errors.New("job already finished")

func (d *Deps) configErr(jc *queue.JobCtx, msg string) error {
	_ = jc.Fail("config", msg, "Fix eve.yml or server.yml; `eve doctor --host` checks the host.", nil)
	return errHandled
}

// logged runs fn with a redacting log file for phase; on failure it classifies the log and ends
// the job.
func (d *Deps) logged(ws *workspace, phase string, state api.State, fn func(log io.Writer) error) error {
	path := ws.jc.LogPath(phase)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w := d.RedactLog(f, ws.secrets)
	runErr := fn(w)
	_ = w.Close()
	_ = f.Close()
	if runErr == nil {
		return nil
	}
	if errors.Is(ws.jc.Ctx.Err(), context.Canceled) {
		return ws.jc.Ctx.Err()
	}
	d.failFromLog(ws.jc, phase, state, path, runErr, ws.app)
	return errHandled
}

func (d *Deps) failFromLog(jc *queue.JobCtx, phase string, state api.State, path string, runErr error, app config.App) {
	b, _ := os.ReadFile(path)
	d.failFrom(jc, phase, state, b, runErr, app)
}

// failFrom classifies log, the failed phase's output, and ends the job.
func (d *Deps) failFrom(jc *queue.JobCtx, phase string, state api.State, b []byte, runErr error, app config.App) {
	exit := -1
	var ee *xcode.ExitError
	if errors.As(runErr, &ee) {
		exit = ee.ExitCode
	}
	var f *api.Failure
	var h *api.Human
	if d.Classifier != nil {
		f, h = d.Classifier.Classify(state, b, exit, map[string]string{"signing": app.SigningMode(), "phase": phase})
	}
	if f == nil {
		f = &api.Failure{Class: "unknown", Summary: runErr.Error(), By: "rules", Excerpt: lastLines(b, 20)}
	}
	if f.Log == "" {
		f.Log = fmt.Sprintf("eve logs %s --phase %s --errors", jc.Job.ID, phase)
	}
	endWith(jc, f, h)
}

// endWith ends the job as needs_human when the classifier named a human action, else failed.
func endWith(jc *queue.JobCtx, f *api.Failure, h *api.Human) {
	if h != nil {
		_ = jc.NeedsHuman(h.Action, h.Detail, h.URL, func(j *store.Job) { j.Result.Failure = f })
		return
	}
	_ = jc.FailWith(f, nil)
}

func lastLines(b []byte, n int) []string {
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if len(l) > 200 {
			l = l[:200]
		}
		out = append(out, string(l))
	}
	return out
}
