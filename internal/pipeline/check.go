package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/self-deprecated/eve/internal/xcode"
)

// Check builds the app for the Simulator, signed ad hoc, and runs the scheme's tests on a fresh
// Simulator when eve.yml asks for them (PLAN.md §6.5). It never touches App Store Connect or
// the login keychain.
type Check struct{ *Deps }

// NewCheck returns the check pipeline.
func NewCheck(d *Deps) *Check {
	d.defaults()
	return &Check{d}
}

// maxFailures bounds the failing tests a result lists.
const maxFailures = 5

// Run builds, and tests if configured. A restart repeats the whole check.
func (c *Check) Run(jc *queue.JobCtx) error {
	if err := jc.EnterWith(api.StatePreparing, "preparing workspace", recordSource); err != nil {
		return err
	}
	ws, err := c.prepare(jc)
	if err != nil {
		return swallow(err)
	}
	spec := xcode.SimSpec{
		Dir: ws.projectDir, Project: ws.project, Scheme: ws.repo.Project.Scheme, XCConfig: ws.xcconfig,
		DerivedData: filepath.Join(c.Config.DataDir, "derived", jc.Job.App, "check"),
	}
	if ws.repo.Check.Test {
		return swallow(c.test(jc, ws, spec))
	}
	if err := jc.Enter(api.StateBuilding, "building for the Simulator"); err != nil {
		return err
	}
	if err := c.logged(ws, "build", api.StateBuilding, func(log io.Writer) error {
		return ws.tools.SimulatorBuild(jc.Ctx, spec, log)
	}); err != nil {
		return swallow(err)
	}
	return jc.Succeed("builds for the Simulator", nil)
}

// Watch is never called: check jobs do not park.
func (c *Check) Watch(*queue.JobCtx) error { return nil }

func (c *Check) test(jc *queue.JobCtx, ws *workspace, spec xcode.SimSpec) error {
	if err := jc.Enter(api.StateTesting, "creating a fresh Simulator"); err != nil {
		return err
	}
	if n, _ := ws.tools.DeleteOrphans(jc.Ctx, xcode.SimulatorPrefix); n > 0 {
		jc.Note(fmt.Sprintf("deleted %d Simulator(s) left behind by an interrupted check", n))
	}
	runtimes, err := ws.tools.Runtimes(jc.Ctx)
	if err != nil {
		return c.simErr(jc, err)
	}
	runtime, deviceType, err := xcode.PickSimulator(runtimes, ws.repo.Check.Simulator)
	if err != nil {
		return c.simErr(jc, err)
	}
	id, err := ws.tools.CreateSimulator(jc.Ctx, xcode.SimulatorPrefix+jc.Job.ID, deviceType, runtime)
	if id != "" {
		defer func() { _ = ws.tools.DeleteSimulator(jc.Ctx, id) }()
	}
	if err != nil {
		return c.simErr(jc, err)
	}
	sim := simLabel(deviceType, runtime)
	jc.Note("building and testing on " + sim)

	spec.SimulatorID = id
	spec.ResultBundle = filepath.Join(jc.Dir, "Tests.xcresult")
	_ = os.RemoveAll(spec.ResultBundle)
	logPath := jc.LogPath("test")
	testErr := c.writeLog(ws, logPath, func(log io.Writer) error { return ws.tools.Test(jc.Ctx, spec, log) })
	if errors.Is(jc.Ctx.Err(), context.Canceled) {
		return jc.Ctx.Err()
	}

	var tests *api.Tests
	sum, sumErr := xcode.TestSummary{}, errors.New("no result bundle")
	if fileExists(filepath.Join(spec.ResultBundle, "Info.plist")) {
		sum, sumErr = ws.tools.ReadTestSummary(jc.Ctx, spec.ResultBundle)
	}
	if sumErr == nil {
		tests = testsFrom(sum, sim)
		if err := jc.Update(func(j *store.Job) { j.Result.Tests = tests }); err != nil {
			return err
		}
	}
	switch {
	case tests != nil && tests.Failed > 0:
		return jc.FailWith(testFailure(jc.Job.ID, tests), nil)
	case testErr != nil:
		// The tests did not compile, did not start, or crashed the runner.
		c.failFromLog(jc, "test", api.StateTesting, logPath, testErr, ws.app)
		return errHandled
	case tests == nil:
		_ = jc.Fail("test.no_results", "the tests ran but left no readable result bundle: "+sumErr.Error(),
			"eve logs "+jc.Job.ID+" --phase test", nil)
		return errHandled
	}
	msg := fmt.Sprintf("%d tests passed", tests.Passed)
	if tests.Skipped > 0 {
		msg += fmt.Sprintf(", %d skipped", tests.Skipped)
	}
	return jc.Succeed(msg, nil)
}

// writeLog runs fn with a redacting log file at path, without classifying a failure.
func (c *Check) writeLog(ws *workspace, path string, fn func(io.Writer) error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w := c.RedactLog(f, ws.secrets)
	runErr := fn(w)
	_ = w.Close()
	_ = f.Close()
	return runErr
}

func (c *Check) simErr(jc *queue.JobCtx, err error) error {
	if errors.Is(jc.Ctx.Err(), context.Canceled) {
		return jc.Ctx.Err()
	}
	hint := "Check the Simulator runtimes on Quartz (`xcrun simctl list runtimes`); eve.yml's check.simulator names a device type."
	if strings.Contains(err.Error(), "bootstatus") {
		// Seen when Quartz was out of memory: first-boot data migration never finished.
		hint = "The Simulator never finished booting. Quartz may be short of memory or CoreSimulator stuck; " +
			"ask the user to check Quartz, then resubmit."
	}
	_ = jc.Fail("simulator.unavailable", "could not get a fresh Simulator: "+err.Error(), hint, nil)
	return errHandled
}

func testsFrom(s xcode.TestSummary, sim string) *api.Tests {
	t := &api.Tests{
		Passed: s.PassedTests, Failed: s.FailedTests, Skipped: s.SkippedTests,
		ExpectedFailures: s.ExpectedFailures, Total: s.TotalTestCount, Simulator: sim,
	}
	for _, f := range s.TestFailures {
		if len(t.Failures) == maxFailures {
			break
		}
		name := f.TestIdentifier
		if name == "" {
			name = strings.Trim(f.TargetName+"/"+f.TestName, "/")
		}
		t.Failures = append(t.Failures, api.TestFailure{Test: name, Message: oneLine(f.FailureText, 300)})
	}
	return t
}

func testFailure(id string, t *api.Tests) *api.Failure {
	f := &api.Failure{
		Class: "test.failed", By: "rules",
		Summary: fmt.Sprintf("%d of %d tests failed", t.Failed, t.Total),
		Hint:    "Fix the code or the test, then resubmit. tests.failures has the first failures.",
		Log:     "eve logs " + id + " --phase test --errors",
	}
	if len(t.Failures) > 0 {
		first := t.Failures[0]
		f.Summary += "; first: " + first.Test
		if first.Message != "" {
			f.Summary += ": " + first.Message
		}
		for _, x := range t.Failures {
			f.Excerpt = append(f.Excerpt, x.Test+": "+x.Message)
		}
	}
	f.Summary = oneLine(f.Summary, 400)
	return f
}

// simLabel turns identifiers into "iPhone 17 Pro, iOS 26.5".
func simLabel(deviceType, runtime string) string {
	d := strings.ReplaceAll(strings.TrimPrefix(deviceType, "com.apple.CoreSimulator.SimDeviceType."), "-", " ")
	r := strings.TrimPrefix(runtime, "com.apple.CoreSimulator.SimRuntime.")
	if os, ver, ok := strings.Cut(r, "-"); ok {
		r = os + " " + strings.ReplaceAll(ver, "-", ".")
	}
	return d + ", " + r
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// recordSource copies the request's source description into the result.
func recordSource(j *store.Job) {
	s := j.Request.Source
	j.Result.Source = api.SourceInfo{SHA256: s.SHA256, Path: s.Path, Commit: s.Commit, Branch: s.Branch, Dirty: s.Dirty}
}
