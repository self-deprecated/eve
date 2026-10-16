package run

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestExecExitCodeAndEnv(t *testing.T) {
	var out bytes.Buffer
	res, err := Exec{}.Run(context.Background(), Cmd{
		Name: "/bin/sh", Args: []string{"-c", "echo $FOO; exit 3"},
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
		Name: "/bin/sh", Args: []string{"-c", "echo \"[$EVE_LEAK_CHECK]\""}, Stdout: &out,
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
		Name: "/bin/sh", Args: []string{"-c", "trap '' INT; sleep 30 & wait"},
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
		Name: "/bin/sleep", Args: []string{"10"}, Timeout: 200 * time.Millisecond,
	})
	if err != ErrTimeout {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
}
