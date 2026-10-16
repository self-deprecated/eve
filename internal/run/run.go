// Package run executes external commands for the pipeline. Every command runs in its own process
// group so cancellation and restart recovery can kill the whole tree (PLAN.md §4.1), with an
// explicit environment and never through a shell that reads startup files.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Cmd describes one command.
type Cmd struct {
	Name    string
	Args    []string
	Dir     string
	Env     []string // complete environment; nothing is inherited
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Timeout time.Duration // 0 = no timeout beyond the context
	// OnStart is called with the process group id as soon as the process has started.
	OnStart func(pgid int)
}

// String renders the command line for logs.
func (c Cmd) String() string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

// Result is the outcome of a command that ran.
type Result struct {
	ExitCode int
	Pgid     int
	Duration time.Duration
}

// ErrTimeout is returned when Cmd.Timeout elapsed.
var ErrTimeout = errors.New("command timed out")

// Runner runs commands. Tests use Fake.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// Exec is the real Runner.
type Exec struct {
	// Grace is how long a cancelled process group gets after SIGINT before SIGKILL.
	Grace time.Duration
}

// Run starts c in a new process group and waits for it. A non-zero exit is reported in
// Result.ExitCode with a nil error; errors mean the command could not run, timed out or was
// cancelled.
func (e Exec) Run(ctx context.Context, c Cmd) (Result, error) {
	grace := e.Grace
	if grace == 0 {
		grace = 20 * time.Second
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.Command(c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("start %s: %w", c.Name, err)
	}
	pgid := cmd.Process.Pid
	if c.OnStart != nil {
		c.OnStart(pgid)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGINT)
		select {
		case waitErr = <-done:
		case <-time.After(grace):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			waitErr = <-done
		}
		// The leader is gone; make sure nothing else in its group survives.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		res := Result{ExitCode: exitCode(waitErr), Pgid: pgid, Duration: time.Since(start)}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, ErrTimeout
		}
		return res, ctx.Err()
	}
	res := Result{ExitCode: exitCode(waitErr), Pgid: pgid, Duration: time.Since(start)}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return res, waitErr
	}
	return res, nil
}

// KillGroup stops a process group that is not a child of this process (an orphan from a
// previous server run): SIGINT, then SIGKILL if anything is still alive after grace.
func KillGroup(pgid int, grace time.Duration) {
	if pgid <= 1 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGINT)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !GroupAlive(pgid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// GroupAlive reports whether any process in the group still exists.
func GroupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exitErr.ExitCode()
	}
	return -1
}
