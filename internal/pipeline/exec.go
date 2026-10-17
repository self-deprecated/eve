package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/self-deprecated/eve/internal/xcode"
)

// Exec runs a repository command in the job's workspace on Quartz, inside the GUI session and
// with the child environment, so a script that drives the Simulator needs no SSH or rsync of its
// own (PLAN.md §6.6). Each top-level entry of $EVE_ARTIFACTS becomes one of the job's artifacts.
type Exec struct{ *Deps }

// NewExec returns the exec pipeline.
func NewExec(d *Deps) *Exec {
	d.defaults()
	return &Exec{d}
}

// artifactName is what the server will serve (the same rule as its artifact route).
var artifactName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}$`)

// Run prepares the workspace (XcodeGen and pre commands included), then runs the command. A
// restart runs it again from the start.
func (e *Exec) Run(jc *queue.JobCtx) error {
	if err := jc.EnterWith(api.StatePreparing, "preparing workspace", recordSource); err != nil {
		return err
	}
	ws, err := e.prepare(jc)
	if err != nil {
		return swallow(err)
	}
	argv := jc.Job.Request.Options.Command
	if len(argv) == 0 {
		return swallow(e.configErr(jc, "exec needs a command"))
	}
	if !ws.app.AllowExec {
		return swallow(e.configErr(jc, fmt.Sprintf("app %s does not allow exec (allowExec in server.yml)", jc.Job.App)))
	}
	dir := filepath.Join(jc.Dir, "artifacts")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := jc.Enter(api.StateRunning, "running "+strings.Join(argv, " ")); err != nil {
		return err
	}
	name := argv[0]
	if strings.Contains(name, "/") && !filepath.IsAbs(name) {
		name = filepath.Join(ws.src, name) // relative to the repository root, as the agent typed it
	}
	env := append(slices.Clone(ws.env), "EVE_ARTIFACTS="+dir, "EVE_JOB="+jc.Job.ID, "EVE_PROJECT_DIR="+ws.projectDir)
	ctx, cancel := context.WithTimeout(jc.Ctx, e.ExecTimeout)
	defer cancel()
	logPath := jc.LogPath("exec")
	runErr := e.writeLog(ws, logPath, func(log io.Writer) error {
		_, _ = fmt.Fprintf(log, "$ %s\n", strings.Join(argv, " "))
		res, err := e.Runner.Run(ctx, run.Cmd{Name: name, Args: argv[1:], Dir: ws.src, Env: env, Stdout: log, Stderr: log, OnStart: jc.OnStart})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return &xcode.ExitError{What: argv[0], ExitCode: res.ExitCode}
		}
		return nil
	})
	if errors.Is(jc.Ctx.Err(), context.Canceled) {
		return jc.Ctx.Err()
	}
	names, skipped := listArtifacts(dir)
	keep := func(j *store.Job) {
		j.Result.Artifacts = names
		if len(skipped) > 0 {
			j.Result.Notes = append(j.Result.Notes, api.Note{Kind: "artifacts", Detail: "not served (symlinks, special files or names other than letters, digits, . _ -): " + strings.Join(skipped, ", ")})
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return jc.Fail("exec.timeout", fmt.Sprintf("%s ran longer than %s and was stopped", argv[0], e.ExecTimeout),
			"Make the command finish sooner, or split it.", keep)
	case runErr != nil:
		b, _ := os.ReadFile(logPath)
		f := &api.Failure{
			Class: "exec.failed", By: "rules", Summary: runErr.Error(), Excerpt: lastLines(b, 20),
			Log: fmt.Sprintf("eve logs %s --phase exec", jc.Job.ID),
		}
		if len(names) > 0 {
			f.Hint = "Artifacts written before it failed are kept: eve artifacts " + jc.Job.ID
		}
		return jc.FailWith(f, keep)
	}
	msg := argv[0] + " succeeded"
	if len(names) > 0 {
		msg += fmt.Sprintf("; %d artifact(s)", len(names))
	}
	return jc.Succeed(msg, keep)
}

// Watch is never called: exec jobs do not park.
func (e *Exec) Watch(*queue.JobCtx) error { return nil }

// listArtifacts returns the servable top-level entries of dir, directories with a trailing "/",
// and the names it skipped.
func listArtifacts(dir string) (names, skipped []string) {
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		n := en.Name()
		switch {
		case !artifactName.MatchString(n):
			skipped = append(skipped, n)
		case en.Type().IsRegular():
			names = append(names, n)
		case en.IsDir():
			names = append(names, n+"/")
		default:
			skipped = append(skipped, n)
		}
	}
	return names, skipped
}
