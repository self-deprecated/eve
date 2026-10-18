//go:build unix

package run

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestExecExitCodeAndEnv(t *testing.T) {
	var out bytes.Buffer
	res, err := Exec{}.Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", "echo $FOO; exit 3"},
		Env: []string{"FOO=bar"}, Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || out.String() != "bar\n" {
		t.Fatalf("got exit %d output %q", res.ExitCode, out.String())
	}
}

func TestExecEnvIsNotInherited(t *testing.T) {
	t.Setenv("EVE_LEAK_CHECK", "leaked")
	var out bytes.Buffer
	if _, err := (Exec{}).Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", "echo \"[$EVE_LEAK_CHECK]\""}, Stdout: &out,
	}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]\n" {
		t.Fatalf("environment leaked: %q", out.String())
	}
}

func TestExecCancelKillsGroupIgnoringSIGINT(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var pgid int
	started := make(chan struct{})
	go func() {
		<-started
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	begin := time.Now()
	// The child ignores SIGINT and spawns a grandchild in the same group.
	_, err := Exec{Grace: 300 * time.Millisecond}.Run(ctx, Cmd{
		Name: "sh", Args: []string{"-c", "trap '' INT; sleep 30 & wait"}, Env: hostPath(),
		OnStart: func(p int) { pgid = p; close(started) },
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(begin) > 5*time.Second {
		t.Fatal("group was not killed promptly")
	}
	time.Sleep(100 * time.Millisecond)
	if GroupAlive(pgid) {
		t.Fatal("process group survived cancellation")
	}
}

func TestExecTimeout(t *testing.T) {
	_, err := Exec{Grace: 100 * time.Millisecond}.Run(context.Background(), Cmd{
		Name: "sleep", Args: []string{"10"}, Timeout: 200 * time.Millisecond,
	})
	if err != ErrTimeout {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
}

func TestExecDoesNotWaitForAnEscapedGrandchildsOutput(t *testing.T) {
	for _, code := range []int{0, 3} {
		escapedGrandchild(t, code)
	}
}

func escapedGrandchild(t *testing.T, code int) {
	t.Helper()
	var out bytes.Buffer
	begin := time.Now()
	// The grandchild leaves the process group and keeps stdout open, as `simctl diagnose`
	// does under xcodebuild.
	res, err := Exec{OutputDelay: 300 * time.Millisecond}.Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", `perl -e 'setpgrp(0,0); print "$$\n"; sleep 30' & sleep 0.2; exit ` + fmt.Sprint(code)},
		Env: hostPath(), Stdout: &out,
	})
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("waited %s for the grandchild's output", took)
	}
	var pid int
	if _, scanErr := fmt.Sscan(out.String(), &pid); scanErr == nil && pid > 1 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if res.ExitCode != code || err != nil {
		t.Fatalf("exit %d, err %v; want exit %d and no error", res.ExitCode, err, code)
	}
}

// hostPath lets test scripts find sleep and perl wherever this host keeps them (not /bin on
// NixOS); the runner itself passes no environment of its own.
func hostPath() []string { return []string{"PATH=" + os.Getenv("PATH")} }
