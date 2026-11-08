package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/asc"
	"github.com/self-deprecated/eve/internal/bridge"
	"github.com/self-deprecated/eve/internal/classify"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/launchd"
	"github.com/self-deprecated/eve/internal/notify"
	"github.com/self-deprecated/eve/internal/pipeline"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/redact"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/server"
	"github.com/self-deprecated/eve/internal/simulator"
	"github.com/self-deprecated/eve/internal/sizeguard"
	"github.com/self-deprecated/eve/internal/source"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/self-deprecated/eve/internal/xcode"
)

// System is a fully wired eve server.
type System struct {
	Config    *config.Server
	Store     *store.Store
	Queue     *queue.Queue
	Server    *server.Server
	Deliverer *notify.Deliverer
	Sources   source.Store
	ASC       *asc.Client
	Bridge    *bridge.Manager
	Log       *slog.Logger
}

// Assemble opens the database and wires every package together.
func Assemble(ctx context.Context, cfg *config.Server, version string, log *slog.Logger) (*System, error) {
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "jobs"), filepath.Join(cfg.DataDir, "sources"), filepath.Join(cfg.DataDir, "logs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "eve.db"))
	if err != nil {
		return nil, err
	}
	ascClient, err := ascFromConfig(cfg)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("loading the App Store Connect key: %w", err)
	}
	sys := &System{
		Config: cfg, Store: st, Sources: source.Store{Dir: filepath.Join(cfg.DataDir, "sources")}, ASC: ascClient,
		Log: log,
	}

	home, _ := os.UserHomeDir()
	deps := &pipeline.Deps{
		Config: cfg, Store: st, Runner: run.Exec{}, Sources: sourcesAdapter{sys.Sources},
		Size: sizeAdapter{}, Classifier: classifierAdapter{},
		RedactLog: func(w io.Writer, secrets []string) io.WriteCloser {
			return redact.New(redact.Options{Home: home, Secrets: secrets}).Writer(w)
		},
	}
	if ascClient != nil {
		deps.ASC = ascClient
	}
	pipelines := map[api.Verb]queue.Pipeline{
		api.VerbTestflight: pipeline.NewTestflight(deps),
		api.VerbCheck:      pipeline.NewCheck(deps),
		api.VerbDevice:     pipeline.NewDevice(deps),
		api.VerbExec:       pipeline.NewExec(deps),
	}
	q := queue.New(st, cfg.DataDir, pipelines, sys.route, log)
	sys.Queue = q
	lifecycle := &simulator.Manager{Store: st, DataDir: cfg.DataDir, Settings: cfg.Simulators}
	var lastSweep time.Time
	q.Maintain = func(ctx context.Context) {
		automatic := cfg.Simulators.IdleAfter() > 0 || cfg.Simulators.DeleteSettledAfter() > 0
		if !automatic || time.Since(lastSweep) < time.Minute {
			return
		}
		lastSweep = time.Now()
		sweep, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		tools := xcode.Tools{Runner: deps.Runner, Env: xcode.ChildEnv(xcode.EnvSpec{Home: home, User: os.Getenv("USER")})}
		if err := lifecycle.Sweep(sweep, tools); err != nil {
			log.Warn("Simulator cleanup deferred", "err", err)
		}
	}

	bridgeTools := xcode.Tools{Runner: deps.Runner, Env: xcode.ChildEnv(xcode.EnvSpec{Home: home, User: os.Getenv("USER")})}
	sys.Bridge = &bridge.Manager{
		DataDir: cfg.DataDir, Settings: cfg.Device.Bridge, Runner: deps.Runner, Log: log,
		Tailscale: func() (string, error) { return findTailscale(cfg.Path) },
		Probe:     func(ctx context.Context) error { return probePhone(ctx, bridgeTools, cfg.Device.Default) },
	}

	sys.Deliverer = &notify.Deliverer{
		Outbox: outboxAdapter{st}, Secrets: notify.ConfigSecrets{Notify: cfg.Notify}, AllowHosts: cfg.Notify.AllowHosts,
		Version: version, Logger: log,
		OnExhausted: func(d notify.Delivery) {
			if tgt, ok := notify.ExhaustedCopy(d, cfg.Notify.Human); ok {
				_ = st.Enqueue(context.Background(), d.JobID, store.NewDelivery{
					Event:      notify.EventDeliveryExhausted,
					TargetKind: tgt.Kind, TargetAddress: tgt.Address, Payload: d.Payload,
				})
			}
		},
		OnError: func(d notify.Delivery, err error) {
			log.Warn("notification failed", "job", d.JobID, "target", d.Target.Kind, "err", err)
		},
	}
	q.AfterCommit = sys.Deliverer.Kick

	sys.Server = &server.Server{
		Config: cfg, Store: st, Queue: q, Sources: sys.Sources, Version: version, Log: log,
		AllowURL:    notify.AllowedURL,
		SizeDetails: sys.sizeDetails,
		Doctor:      sys.Doctor,
		ErrorLines:  func(b []byte, max int) []string { return classify.Excerpt(b, max) },
		Bridge:      sys.Bridge,
	}
	if ascClient != nil {
		sys.Server.Builds = sys.builds
	}
	return sys, nil
}

// Close releases the database.
func (s *System) Close() { _ = s.Store.Close() }

// route decides who hears about a job entering a state (docs/design.md §8).
func (s *System) route(j store.Job, st api.State) []store.NewDelivery {
	event := api.NotifyEventFor(st)
	if event == "" {
		return nil
	}
	n := api.Notification{Event: event, ID: j.ID, Result: j.Result}
	var out []store.NewDelivery
	for _, t := range notify.BuildDeliveries(n, j.Request.NotifyURLs, s.Config.Notify.Human, event) {
		out = append(out, store.NewDelivery{Event: event, TargetKind: t.Kind, TargetAddress: t.Address, Payload: n})
	}
	return out
}

// EnsureOwnerToken creates the owner token on first start (docs/design.md §4.2).
func (s *System) EnsureOwnerToken(ctx context.Context) error {
	path := filepath.Join(s.Config.DataDir, "token")
	if b, err := os.ReadFile(path); err == nil {
		if _, err := s.Store.VerifyToken(ctx, strings.TrimSpace(string(b))); err == nil {
			return nil
		}
	}
	tok, err := s.Store.CreateToken(ctx, "owner", true)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return err
	}
	s.Log.Info("created the owner token", "path", path)
	return nil
}

// probePhone makes one device call that needs a tunnel to the phone eve installs on by default
// (device.default, else the first paired physical iPhone).
func probePhone(ctx context.Context, tools xcode.Tools, want string) error {
	devices, err := tools.Devices(ctx)
	if err != nil {
		return err
	}
	for _, d := range devices {
		if d.Physical && d.Platform == "iOS" && (want != "" && d.Matches(want) || want == "" && d.Paired && d.Type == "iPhone") {
			return tools.LockState(ctx, d.ID)
		}
	}
	return errors.New("no paired iPhone is known to the build Mac")
}

// findTailscale finds the tailscale CLI: eve serve's PATH has only Apple's tools.
func findTailscale(extra []string) (string, error) {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, nil
	}
	dirs := append(append([]string{}, extra...), "/run/current-system/sw/bin", "/etc/profiles/per-user/"+os.Getenv("USER")+"/bin", "/opt/homebrew/bin", "/usr/local/bin")
	for _, d := range dirs {
		if p := filepath.Join(d, "tailscale"); fileExecutable(p) {
			return p, nil
		}
	}
	if p := "/Applications/Tailscale.app/Contents/MacOS/Tailscale"; fileExecutable(p) {
		return p, nil
	}
	return "", errors.New("the tailscale CLI isn't installed where eve looks (PATH, server.yml path, /run/current-system/sw/bin, Homebrew, Tailscale.app)")
}

func fileExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// Housekeep prunes old uploads and job workspaces.
func (s *System) Housekeep(ctx context.Context) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		if n, err := s.Sources.Prune(7 * 24 * time.Hour); err == nil && n > 0 {
			s.Log.Info("pruned uploads", "count", n)
		}
		s.pruneWorkspaces(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pruneWorkspaces removes the bulky parts of finished jobs older than a week (sources and
// archives), keeping logs, results and manifests, and the archive of each app's baseline job.
func (s *System) pruneWorkspaces(ctx context.Context) {
	jobs, err := s.Store.List(ctx, store.Filter{Limit: 500})
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for app := range s.Config.Apps {
		if b, _ := s.Store.Baseline(ctx, app); b != nil {
			keep[b.JobID] = true
		}
	}
	for _, j := range jobs {
		if !j.State.Terminal() || time.Since(j.FinishedAt) < 7*24*time.Hour {
			continue
		}
		dir := filepath.Join(s.Config.DataDir, "jobs", j.ID)
		for _, sub := range []string{"src", "unpacked", "tmp", "export", "secrets.xcconfig"} {
			_ = os.RemoveAll(filepath.Join(dir, sub))
		}
		if !keep[j.ID] {
			matches, _ := filepath.Glob(filepath.Join(dir, "*.xcarchive"))
			for _, m := range matches {
				_ = os.RemoveAll(m)
			}
		}
	}
}

func (s *System) sizeDetails(ctx context.Context, j store.Job) (any, error) {
	cur, err := sizeguard.LoadManifest(s.manifestPath(j.ID))
	if err != nil {
		return nil, fmt.Errorf("job %s has no bundle manifest (it never got past archiving)", j.ID)
	}
	var base *sizeguard.Manifest
	if id := s.measuredAgainst(ctx, j); id != "" {
		if m, err := sizeguard.LoadManifest(s.manifestPath(id)); err == nil {
			base = &m
		}
	}
	return map[string]any{"report": j.Result.Size, "diff": sizeguard.Diff(cur, base)}, nil
}

func (s *System) manifestPath(jobID string) string {
	return filepath.Join(s.Config.DataDir, "jobs", jobID, "bundle-manifest.json.zst")
}

// measuredAgainst is the job whose bundle j was measured against. Not the app's current
// baseline: a build that succeeds becomes the baseline itself.
func (s *System) measuredAgainst(ctx context.Context, j store.Job) string {
	r := j.Result.Size
	switch {
	case r == nil:
		return ""
	case r.BaselineJob != "":
		return r.BaselineJob
	case r.BaselineBuild == "":
		return "" // the first build: nothing to compare with
	}
	// Reports from before BaselineJob was recorded name only the build number.
	jobs, err := s.Store.List(ctx, store.Filter{App: j.App, Limit: 500})
	if err != nil {
		return ""
	}
	for _, o := range jobs {
		if o.ID != j.ID && o.BuildNumber == r.BaselineBuild {
			return o.ID
		}
	}
	return ""
}

func (s *System) builds(ctx context.Context, app string) (any, error) {
	a := s.Config.Apps[app]
	appID := a.ASCAppID
	if appID == "" {
		found, err := s.ASC.AppByBundleID(ctx, a.BundleID)
		if err != nil {
			return nil, err
		}
		appID = found.ID
	}
	latest, _, err := s.ASC.LatestBuildNumber(ctx, appID, "")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"app": app, "ascAppId": appID, "latestBuildNumber": latest}
	if latest != "" {
		if b, err := s.ASC.FindBuild(ctx, appID, "", latest); err == nil && b != nil {
			out["latest"] = map[string]any{
				"number": b.Number, "processingState": b.ProcessingState, "audience": b.BuildAudienceType,
				"internalBuildState": betaState(b), "verdict": asc.BuildVerdict(b).Summary,
			}
		}
	}
	return out, nil
}

func betaState(b *asc.Build) string {
	if b.BetaDetail == nil {
		return ""
	}
	return b.BetaDetail.InternalBuildState
}

// Check is one doctor result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
}

// Doctor runs the host checks of docs/design.md §6.8.
func (s *System) Doctor(ctx context.Context) any {
	var out []Check
	add := func(name, status, detail string) { out = append(out, Check{name, status, detail}) }
	cmd := func(name string, args ...string) (string, error) {
		c := exec.CommandContext(ctx, name, args...)
		c.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + os.Getenv("HOME")}
		b, err := c.CombinedOutput()
		return strings.TrimSpace(string(b)), err
	}

	if launchd.InAquaSession() {
		add("gui session", "ok", "running in the Aqua session")
	} else {
		add("gui session", "fail", "not in the GUI session: signing with the Xcode account will fail")
	}
	if outp, err := cmd("/usr/bin/security", "show-keychain-info", filepath.Join(os.Getenv("HOME"), "Library/Keychains/login.keychain-db")); err != nil {
		add("login keychain", "fail", "locked or unavailable: "+firstLine(outp))
	} else {
		add("login keychain", "ok", "unlocked")
	}
	if outp, err := cmd("/usr/bin/security", "find-identity", "-v", "-p", "codesigning"); err == nil && !strings.Contains(outp, " 0 valid") {
		add("signing identities", "ok", lastLine(outp))
	} else {
		add("signing identities", "fail", "no valid codesigning identity")
	}
	if outp, err := cmd("/usr/bin/xcodebuild", "-version"); err == nil {
		add("xcode", "ok", strings.ReplaceAll(outp, "\n", ", "))
	} else {
		add("xcode", "fail", firstLine(outp))
	}
	if p, err := exec.LookPath("rsync"); err == nil && p != "/usr/bin/rsync" {
		add("rsync", "warn", "server PATH resolves rsync to "+p+" (children always get /usr/bin first)")
	} else {
		add("rsync", "ok", "/usr/bin/rsync")
	}
	if len(s.Config.Path) > 0 {
		var missing []string
		for _, dir := range s.Config.Path {
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				missing = append(missing, dir)
			}
		}
		if len(missing) > 0 {
			add("tool path", "fail", "not a directory: "+strings.Join(missing, ", "))
		} else {
			add("tool path", "ok", strings.Join(s.Config.Path, ":"))
		}
	}
	if s.ASC != nil {
		h, err := s.ASC.Ping(ctx)
		status := "ok"
		if h != asc.HealthOK {
			status = "fail"
		}
		detail := string(h)
		if err != nil {
			detail += ": " + err.Error()
		}
		add("app store connect", status, detail)
	} else {
		add("app store connect", "warn", "no API key configured (asc.keyId)")
	}
	if s.Bridge != nil {
		b := s.Bridge.Status()
		switch {
		case b.On && b.Reachable:
			add("phone bridge", "ok", fmt.Sprintf("on until %s to %s; the phone answered through it", b.Until.Local().Format(time.DateTime), b.Phone))
		case b.On:
			add("phone bridge", "warn", b.Detail)
		case b.AdvertSeenAt.IsZero():
			add("phone bridge", "warn", "off; "+bridge.ErrNoAdvert.Error())
		default:
			add("phone bridge", "ok", "off; ready to start: the phone was last seen on this network "+b.AdvertSeenAt.Local().Format(time.DateTime))
		}
	}
	var stfs syscallStatfs
	if free, ok := stfs.free(s.Config.DataDir); ok {
		status := "ok"
		if free < 20e9 {
			status = "warn"
		}
		add("disk", status, fmt.Sprintf("%.0f GB free in %s", float64(free)/1e9, s.Config.DataDir))
	}
	return out
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// --- adapters between the packages ---

type sourcesAdapter struct{ st source.Store }

func (a sourcesAdapter) Extract(sha, dest string) error {
	f, err := a.st.Open(sha)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return source.Extract(f, dest, source.DefaultLimits)
}

func (a sourcesAdapter) Clone(src, dst string) error { return source.Clone(src, dst) }

type sizeAdapter struct{}

func (sizeAdapter) Measure(appDir, manifestPath string) error {
	m, err := sizeguard.BuildManifest(appDir)
	if err != nil {
		return err
	}
	return sizeguard.SaveManifest(manifestPath, m)
}

func (sizeAdapter) Check(manifestPath, baselinePath, baselineBuild string, th config.BundleThresholds, expect bool, details string) (pipeline.SizeVerdict, error) {
	cur, err := sizeguard.LoadManifest(manifestPath)
	if err != nil {
		return pipeline.SizeVerdict{}, err
	}
	var base *sizeguard.Manifest
	if baselinePath != "" {
		if m, err := sizeguard.LoadManifest(baselinePath); err == nil {
			if m.Build == "" {
				m.Build = baselineBuild
			}
			base = &m
		}
	}
	v := sizeguard.CheckBundle(cur, base, th, expect)
	v.Report.Details = details
	if baselineBuild != "" && v.Report.BaselineBuild == "" {
		v.Report.BaselineBuild = baselineBuild
	}
	return pipeline.SizeVerdict{Report: v.Report, Secret: v.Failed, Trip: v.Tripped}, nil
}

type classifierAdapter struct{}

func (classifierAdapter) Classify(phase api.State, log []byte, exit int, facts map[string]string) (*api.Failure, *api.Human) {
	v := classify.Diagnose(classify.Input{Phase: phase, Log: log, ExitCode: exit, Context: facts})
	return v.Failure, v.Human
}

func (classifierAdapter) IsDuplicateUpload(log []byte) bool { return classify.IsDuplicateUpload(log) }

type outboxAdapter struct{ st *store.Store }

func (o outboxAdapter) Due(ctx context.Context, now time.Time, limit int) ([]notify.Delivery, error) {
	rows, err := o.st.OutboxDue(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	out := make([]notify.Delivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, notify.Delivery{
			ID: r.ID, JobID: r.JobID, Event: r.Event,
			Target: notify.Target{Kind: r.TargetKind, Address: r.TargetAddress}, Payload: r.Payload,
			Attempts: r.Attempts, CreatedAt: r.CreatedAt, LastError: r.LastError,
		})
	}
	return out, nil
}

func (o outboxAdapter) Delivered(ctx context.Context, id int64) error {
	return o.st.OutboxDelivered(ctx, id)
}

func (o outboxAdapter) Retry(ctx context.Context, id int64, attempt int, next time.Time, lastErr string) error {
	return o.st.OutboxRetry(ctx, id, attempt, next, lastErr)
}

func (o outboxAdapter) Exhausted(ctx context.Context, id int64, lastErr string) error {
	return o.st.OutboxExhausted(ctx, id, lastErr)
}
