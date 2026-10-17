package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/asc"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/self-deprecated/eve/internal/xcode"
)

// Testflight archives, size-checks, uploads and follows a build until internal testers can
// install it (PLAN.md §6.3).
type Testflight struct{ *Deps }

// NewTestflight returns the testflight pipeline.
func NewTestflight(d *Deps) *Testflight {
	d.defaults()
	return &Testflight{d}
}

// uploadIntent is persisted before delivery so a crash mid-upload is reconciled, never blindly
// repeated (PLAN.md §5.5).
type uploadIntent struct {
	AppID   string `json:"appId"`
	Version string `json:"version"`
	Number  string `json:"number"`
	Archive string `json:"archive"`
}

func (t *Testflight) archivePath(jc *queue.JobCtx) string {
	return filepath.Join(jc.Dir, filepath.Base(jc.Job.App)+".xcarchive")
}

func (t *Testflight) manifestPath(jc *queue.JobCtx) string {
	return filepath.Join(jc.Dir, "bundle-manifest.json.zst")
}

// Run is the worker part: everything up to handing the build to App Store Connect.
func (t *Testflight) Run(jc *queue.JobCtx) error {
	resume := jc.Job.ResumeFrom
	if err := jc.EnterWith(api.StatePreparing, "preparing workspace", recordSource); err != nil {
		return err
	}
	ws, err := t.prepare(jc)
	if err != nil {
		return swallow(err)
	}
	appID, err := t.appID(jc, ws.app)
	if err != nil {
		return swallow(err)
	}

	archive := t.archivePath(jc)
	archived := fileExists(filepath.Join(archive, "Info.plist")) && jc.Job.BuildNumber != ""
	skipToDelivery := archived && (resume == api.StateDelivering || resume == api.StateMeasuring)
	if !skipToDelivery {
		if err := t.archive(jc, ws, appID, archive); err != nil {
			return swallow(err)
		}
	}

	switch {
	case jc.Job.UploadIntent != "" && resume == api.StateDelivering:
		// The server stopped mid-upload: find out what Apple has before doing anything.
		return swallow(t.reconcileAfterCrash(jc, ws, appID, archive))
	case resume == api.StateDelivering && jc.Job.Confirmation != "":
		// The agent confirmed a size-guard pause; the bundle was already measured.
	default:
		paused, err := t.measure(jc, ws, archive)
		if err != nil || paused {
			return swallow(err)
		}
	}
	return swallow(t.deliver(jc, ws, appID, archive, false))
}

func (t *Testflight) appID(jc *queue.JobCtx, app config.App) (string, error) {
	if app.ASCAppID != "" || t.ASC == nil {
		return app.ASCAppID, nil
	}
	a, err := t.ASC.AppByBundleID(jc.Ctx, app.BundleID)
	if err != nil {
		return "", t.ascErr(jc, "looking up the app", err)
	}
	return a.ID, nil
}

func (t *Testflight) archive(jc *queue.JobCtx, ws *workspace, appID, archive string) error {
	number := jc.Job.BuildNumber
	if number == "" {
		floor := ""
		if t.ASC != nil && appID != "" {
			latest, ok, err := t.ASC.LatestBuildNumber(jc.Ctx, appID, "")
			if err != nil {
				return t.ascErr(jc, "reading the latest build number", err)
			}
			if ok {
				floor = incr(latest)
			}
		}
		n, err := t.Store.AllocateBuildNumber(context.Background(), jc.Job.App, floor, jc.Job.Request.Options.BuildNumber)
		if err != nil {
			_ = jc.Fail("build.number", err.Error(), "Omit --build-number to let eve allocate one.", nil)
			return errHandled
		}
		number = n
	}
	_ = os.RemoveAll(archive)
	if err := jc.EnterWith(api.StateArchiving, "archiving build "+number, func(j *store.Job) {
		j.BuildNumber = number
		j.Result.Build = &api.BuildInfo{Number: number}
	}); err != nil {
		return err
	}
	var auth *xcode.APIKey
	if ws.app.SigningMode() == config.SigningAPIKey {
		auth = &xcode.APIKey{Path: t.Config.ASC.KeyPath, ID: t.Config.ASC.KeyID, Issuer: t.Config.ASC.IssuerID}
	}
	spec := xcode.ArchiveSpec{
		Dir: ws.projectDir, Project: ws.project, Scheme: ws.repo.Project.Scheme, ArchivePath: archive,
		DerivedData: filepath.Join(t.Config.DataDir, "derived", jc.Job.App, "testflight"),
		BuildNumber: number, XCConfig: ws.xcconfig, Auth: auth,
	}
	if err := t.logged(ws, "archive", api.StateArchiving, func(log io.Writer) error { return ws.tools.Archive(jc.Ctx, spec, log) }); err != nil {
		return err
	}
	app, err := xcode.ArchivedApp(archive)
	if err != nil {
		_ = jc.Fail("build.archive", err.Error(), "", nil)
		return errHandled
	}
	versions, err := ws.tools.BundleVersions(jc.Ctx, app)
	if err != nil {
		_ = jc.Fail("build.archive", "reading the archived app's versions: "+err.Error(), "", nil)
		return errHandled
	}
	if bad := xcode.CheckBuildNumbers(versions, number); len(bad) > 0 {
		_ = jc.FailWith(&api.Failure{
			Class: "build.number_mismatch", By: "rules",
			Summary: "Not every bundle in the archive carries build number " + number + ".",
			Hint:    "Set CFBundleVersion to $(CURRENT_PROJECT_VERSION) in each target's Info.plist.",
			Excerpt: bad,
		}, nil)
		return errHandled
	}
	short := versions[filepath.Base(app)][1]
	return jc.Update(func(j *store.Job) {
		j.BuildVersion = short
		j.Result.Build = &api.BuildInfo{Version: short, Number: number}
	})
}

// measure runs the bundle check; paused reports that the job now awaits confirmation.
func (t *Testflight) measure(jc *queue.JobCtx, ws *workspace, archive string) (paused bool, err error) {
	if t.Size == nil {
		return false, nil
	}
	if err := jc.Enter(api.StateMeasuring, "measuring the app bundle"); err != nil {
		return false, err
	}
	app, err := xcode.ArchivedApp(archive)
	if err != nil {
		return false, err
	}
	manifest := t.manifestPath(jc)
	if err := t.Size.Measure(app, manifest); err != nil {
		_ = jc.Fail("size.measure", "measuring the bundle: "+err.Error(), "", nil)
		return false, errHandled
	}
	base, err := t.Store.Baseline(context.Background(), jc.Job.App)
	if err != nil {
		return false, err
	}
	basePath, baseBuild := "", ""
	if base != nil && fileExists(base.Path) {
		basePath, baseBuild = base.Path, base.BuildNumber
	}
	th := t.Config.SizeGuardFor(jc.Job.App)
	v, err := t.Size.Check(manifest, basePath, baseBuild, th.Bundle, jc.Job.Request.Options.ExpectSizeChange != "",
		"eve size "+jc.Job.ID)
	if err != nil {
		_ = jc.Fail("size.check", err.Error(), "", nil)
		return false, errHandled
	}
	report := v.Report
	if basePath != "" {
		report.BaselineJob = base.JobID
	}
	switch {
	case v.Secret:
		report.Gate = api.GateFailed
		_ = jc.FailWith(&api.Failure{
			Class: "bundle.secret_material", By: "rules",
			Summary: "The app bundle contains secret material; nothing was uploaded.",
			Hint:    "Remove the file from the target's resources. This cannot be confirmed away.",
			Excerpt: report.Reasons,
		}, func(j *store.Job) { j.Result.Size = &report })
		return false, errHandled
	case v.Trip:
		report.Gate = api.GateAwaitingConfirmation
		deadline := t.Now().Add(time.Duration(th.ConfirmTimeout))
		return true, jc.Park(api.StateAwaitingConfirmation, deadline, "size guard: "+report.Summary, func(j *store.Job) {
			j.Result.Size = &report
		})
	}
	return false, jc.Update(func(j *store.Job) { j.Result.Size = &report })
}

// deliver signs and uploads in one xcodebuild call, then parks the job for processing.
// retried is set for the single re-run after an inconclusive reconciliation.
func (t *Testflight) deliver(jc *queue.JobCtx, ws *workspace, appID, archive string, retried bool) error {
	intent, _ := json.Marshal(uploadIntent{AppID: appID, Version: jc.Job.BuildVersion, Number: jc.Job.BuildNumber, Archive: archive})
	if err := jc.EnterWith(api.StateDelivering, "signing and uploading build "+jc.Job.BuildNumber, func(j *store.Job) {
		j.UploadIntent = string(intent)
	}); err != nil {
		return err
	}
	options := filepath.Join(jc.Dir, "ExportOptions.plist")
	if err := os.WriteFile(options, xcode.ExportOptionsPlist(xcode.ExportSpec{
		Destination: "upload", Team: t.Config.Team, InternalOnly: ws.app.InternalOnly(), UploadSymbols: ws.app.UploadSymbols(),
	}), 0o600); err != nil {
		return err
	}
	var auth *xcode.APIKey
	if ws.app.SigningMode() == config.SigningAPIKey {
		auth = &xcode.APIKey{Path: t.Config.ASC.KeyPath, ID: t.Config.ASC.KeyID, Issuer: t.Config.ASC.IssuerID}
	}
	logPath := jc.LogPath("delivery")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w := t.RedactLog(f, ws.secrets)
	exportErr := ws.tools.Export(jc.Ctx, archive, options, filepath.Join(jc.Dir, "export"), auth, w)
	_ = w.Close()
	_ = f.Close()
	if exportErr == nil {
		return t.parkProcessing(jc, "uploaded; waiting for App Store Connect to process it")
	}
	if errors.Is(jc.Ctx.Err(), context.Canceled) {
		return jc.Ctx.Err()
	}
	log, _ := os.ReadFile(logPath)
	if t.Classifier != nil && t.Classifier.IsDuplicateUpload(log) {
		// An earlier attempt already landed (PLAN.md §5.5): follow that build.
		return t.parkProcessing(jc, "App Store Connect already has this build; following it")
	}
	var ee *xcode.ExitError
	failed := errors.As(exportErr, &ee)
	f2, human := t.classify(api.StateDelivering, log, exitOf(ee), ws.app)
	if failed && f2 != nil && f2.Class != "unknown" {
		// A definite, classified failure (signing, icons, purpose strings…): nothing to reconcile.
		f2.Log = fmt.Sprintf("eve logs %s --phase delivery --errors", jc.Job.ID)
		endWith(jc, f2, human)
		return errHandled
	}
	// Ambiguous: maybe it uploaded and the tool lied, maybe the network dropped. Ask Apple.
	return t.reconcile(jc, ws, appID, archive, retried, f2)
}

func (t *Testflight) reconcileAfterCrash(jc *queue.JobCtx, ws *workspace, appID, archive string) error {
	jc.Note("the server stopped during delivery; checking App Store Connect before uploading again")
	return t.reconcile(jc, ws, appID, archive, false, nil)
}

func (t *Testflight) reconcile(jc *queue.JobCtx, ws *workspace, appID, archive string, retried bool, why *api.Failure) error {
	if t.ASC == nil || appID == "" {
		if why == nil {
			why = &api.Failure{Class: "unknown", Summary: "delivery failed", By: "rules"}
		}
		_ = jc.FailWith(why, nil)
		return errHandled
	}
	r, err := t.ASC.ReconcileUntil(jc.Ctx, appID, jc.Job.BuildVersion, jc.Job.BuildNumber, t.ReconcileWindow, t.ReconcileInterval)
	if err != nil {
		return t.ascErr(jc, "checking whether the upload landed", err)
	}
	switch r.Status {
	case asc.Found, asc.UploadLive:
		return t.parkProcessing(jc, "App Store Connect has the upload; waiting for processing")
	case asc.AllFailed:
		if !retried {
			jc.Note("every upload attempt failed at Apple (" + strings.Join(r.Reasons, "; ") + "); uploading again")
			return t.deliver(jc, ws, appID, archive, true)
		}
		_ = jc.FailWith(&api.Failure{
			Class: "upload.failed", By: "rules", Summary: "App Store Connect refused the upload.",
			Excerpt: r.Reasons,
		}, nil, api.StateRejected)
		return errHandled
	default: // NotVisible after the window
		if !retried {
			jc.Note("no trace of the upload after the reconciliation window; uploading once more")
			return t.deliver(jc, ws, appID, archive, true)
		}
		if why == nil {
			why = &api.Failure{
				Class: "upload.not_visible", By: "rules",
				Summary: "The upload never appeared in App Store Connect after two attempts.",
			}
		}
		_ = jc.FailWith(why, nil)
		return errHandled
	}
}

func (t *Testflight) parkProcessing(jc *queue.JobCtx, msg string) error {
	return jc.Park(api.StateProcessing, t.Now().Add(t.ProcessingDeadline), msg, nil)
}

// Watch follows a parked build through processing and internal distribution (PLAN.md §6.3
// steps 5–6). It returns when the job ends, or nil on shutdown so a later watcher resumes.
func (t *Testflight) Watch(jc *queue.JobCtx) error {
	if t.ASC == nil {
		return jc.Fail("asc.unconfigured", "App Store Connect is not configured; cannot follow processing.", "", nil)
	}
	var intent uploadIntent
	_ = json.Unmarshal([]byte(jc.Job.UploadIntent), &intent)
	appID := intent.AppID
	if appID == "" {
		id, err := t.appID(jc, t.Config.Apps[jc.Job.App])
		if err != nil {
			return swallow(err)
		}
		appID = id
	}
	start := t.Now()
	invisibleChecked := false
	for {
		if !jc.Job.DeadlineAt.IsZero() && t.Now().After(jc.Job.DeadlineAt) {
			return jc.Fail("asc.processing_deadline", "App Store Connect did not finish processing the build in time.",
				"Check the build in App Store Connect; eve stopped watching.", nil)
		}
		b, err := t.ASC.FindBuild(jc.Ctx, appID, jc.Job.BuildVersion, jc.Job.BuildNumber)
		if err != nil {
			if jc.Ctx.Err() != nil {
				return nil
			}
			if asc.IsAgreement(err) {
				return jc.NeedsHuman(api.HumanAcceptAgreement, "App Store Connect needs an agreement accepted", "https://appstoreconnect.apple.com/agreements", nil)
			}
			jc.Note("App Store Connect lookup failed (" + err.Error() + "); retrying")
		} else if b == nil && !invisibleChecked && t.Now().Sub(start) > t.ReconcileWindow {
			invisibleChecked = true
			if r, err := t.ASC.ReconcileUntil(jc.Ctx, appID, jc.Job.BuildVersion, jc.Job.BuildNumber, time.Second, time.Second); err == nil && r.Status == asc.AllFailed {
				return jc.FailWith(&api.Failure{Class: "upload.failed", By: "rules", Summary: "App Store Connect refused the upload.", Excerpt: r.Reasons}, nil, api.StateRejected)
			}
		} else if b != nil {
			done, err := t.step(jc, appID, b)
			if done || err != nil {
				return err
			}
		}
		wait := 30 * time.Second
		if t.Now().Sub(start) > 10*time.Minute {
			wait = 2 * time.Minute
		}
		if err := t.Sleep(jc.Ctx, wait); err != nil {
			return nil // shutting down; a later watcher resumes from the persisted state
		}
	}
}

// step acts on one observation of the build; done reports that the job ended.
func (t *Testflight) step(jc *queue.JobCtx, appID string, b *asc.Build) (bool, error) {
	v := asc.BuildVerdict(b)
	switch v.Kind {
	case asc.Wait, asc.Unknown:
		if v.Kind == asc.Unknown {
			jc.Note(v.Summary)
		}
		if b.ProcessingState == "VALID" && jc.Job.State != api.StateDistributing {
			_ = jc.Enter(api.StateDistributing, "processed; waiting for TestFlight")
		}
		return false, nil
	case asc.Done:
		if jc.Job.State != api.StateDistributing {
			_ = jc.Enter(api.StateDistributing, "processed; waiting for TestFlight")
		}
		return false, nil
	case asc.Rejected:
		return true, jc.FailWith(&api.Failure{Class: "asc.rejected", By: "rules", Summary: v.Summary, Excerpt: v.Reasons}, recordBuild(b), api.StateRejected)
	case asc.Failed:
		return true, jc.FailWith(&api.Failure{Class: "asc.failed", By: "rules", Summary: v.Summary, Excerpt: v.Reasons}, recordBuild(b))
	case asc.NeedsHuman:
		return true, jc.NeedsHuman(v.Human, v.Summary, "https://appstoreconnect.apple.com/apps/"+appID+"/testflight/ios", recordBuild(b))
	case asc.Ready:
		return true, t.distribute(jc, appID, b)
	}
	return false, nil
}

func (t *Testflight) distribute(jc *queue.JobCtx, appID string, b *asc.Build) error {
	app := t.Config.Apps[jc.Job.App]
	if err := asc.CheckAudience(b, app.InternalOnly()); err != nil {
		return jc.FailWith(&api.Failure{
			Class: "asc.audience", By: "rules", Summary: err.Error(),
			Hint: "The build was not distributed. Check the export options (testFlightInternalTestingOnly).",
		}, recordBuild(b))
	}
	// done says what reached testers, so the agent can tell the user it is on their phone.
	var notes []api.Note
	var done []string
	if group := app.Testflight.Group; group != "" {
		g, err := t.ASC.BetaGroupByName(jc.Ctx, appID, group)
		switch {
		case err != nil:
			notes = append(notes, api.Note{Kind: "group", Detail: "could not find group " + group + ": " + err.Error()})
		case !g.Internal:
			notes = append(notes, api.Note{Kind: "group", Detail: "group " + group + " is not internal; not adding the build"})
		case g.AutoDistribution:
			done = append(done, "group "+group+" gets every build automatically")
		default:
			if err := t.ASC.AddBuildToGroup(jc.Ctx, g.ID, b.ID); err != nil {
				notes = append(notes, api.Note{Kind: "group", Detail: "adding to " + group + ": " + err.Error()})
			} else {
				done = append(done, "added to group "+group)
			}
		}
	}
	if text := whatToTest(jc.Job); text != "" {
		if _, err := t.ASC.SetWhatToTest(jc.Ctx, b.ID, text); err != nil {
			notes = append(notes, api.Note{Kind: "what_to_test", Detail: err.Error()})
		} else {
			done = append(done, "What to Test set")
		}
	}
	if app.Testflight.Tester != "" {
		inv, err := t.ASC.TesterInviteState(jc.Ctx, appID, app.Testflight.Tester)
		switch {
		case err != nil:
		case inv.State == asc.InviteAccepted:
			done = append(done, app.Testflight.Tester+" has accepted the TestFlight invite")
		default:
			notes = append(notes, api.Note{
				Kind:   api.HumanAcceptTestflightInvite,
				Detail: fmt.Sprintf("%s has not accepted the TestFlight invite (%s); the build will not show up for them until they do", app.Testflight.Tester, inv.AppleState),
			})
		}
	}
	if len(done) > 0 {
		notes = append([]api.Note{{Kind: "distributed", Detail: strings.Join(done, "; ")}}, notes...)
	}
	if err := jc.Succeed("ready for internal testers", func(j *store.Job) {
		recordBuild(b)(j)
		j.Result.Notes = append(j.Result.Notes, notes...)
	}); err != nil {
		return err
	}
	t.accept(jc)
	return nil
}

// accept makes this build's bundle the size baseline and its source the source baseline.
func (t *Testflight) accept(jc *queue.JobCtx) {
	ctx := context.Background()
	if p := t.manifestPath(jc); fileExists(p) {
		_ = t.Store.SetBaseline(ctx, store.Baseline{App: jc.Job.App, JobID: jc.Job.ID, BuildNumber: jc.Job.BuildNumber, Path: p})
	}
	if st := jc.Job.Request.Source.Stats; st.Bytes > 0 {
		_ = t.Store.SetSourceStats(ctx, jc.Job.App, jc.Job.ID, st)
	}
}

func recordBuild(b *asc.Build) func(*store.Job) {
	return func(j *store.Job) {
		if j.Result.Build == nil {
			j.Result.Build = &api.BuildInfo{}
		}
		j.Result.Build.ASCBuildID = b.ID
		if j.Result.Build.Number == "" {
			j.Result.Build.Number = b.Number
		}
	}
}

func whatToTest(j store.Job) string {
	if n := strings.TrimSpace(j.Request.Options.Notes); n != "" {
		return n
	}
	if c := j.Request.Source.Commit; c != "" {
		return "Build " + j.BuildNumber + " from " + c
	}
	return ""
}

func (t *Testflight) classify(state api.State, log []byte, exit int, app config.App) (*api.Failure, *api.Human) {
	if t.Classifier == nil {
		return nil, nil
	}
	return t.Classifier.Classify(state, log, exit, map[string]string{"signing": app.SigningMode()})
}

func (t *Testflight) ascErr(jc *queue.JobCtx, what string, err error) error {
	if errors.Is(jc.Ctx.Err(), context.Canceled) {
		return jc.Ctx.Err()
	}
	switch {
	case asc.IsAgreement(err):
		_ = jc.NeedsHuman(api.HumanAcceptAgreement, "App Store Connect needs an agreement accepted ("+what+")",
			"https://appstoreconnect.apple.com/agreements", nil)
	case errors.Is(err, asc.ErrAuth):
		_ = jc.Fail("asc.auth", "App Store Connect rejected the API key while "+what+".", "Run `eve doctor --host`.", nil)
	case errors.Is(err, asc.ErrUnavailable):
		_ = jc.Fail("asc.unavailable", "App Store Connect is unreachable ("+what+").", "Resubmit later.", nil, api.StateUnavailable)
	default:
		_ = jc.Fail("asc.error", what+": "+err.Error(), "", nil)
	}
	return errHandled
}

// swallow turns errHandled into nil: the job was already ended with a proper failure.
func swallow(err error) error {
	if errors.Is(err, errHandled) {
		return nil
	}
	return err
}

func exitOf(ee *xcode.ExitError) int {
	if ee == nil {
		return -1
	}
	return ee.ExitCode
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func incr(n string) string {
	b := []byte(n)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < '0' || b[i] > '9' {
			return ""
		}
		if b[i] < '9' {
			b[i]++
			return string(b)
		}
		b[i] = '0'
	}
	return "1" + string(b)
}
