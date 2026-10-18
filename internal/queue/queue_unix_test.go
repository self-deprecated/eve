//go:build unix

package queue

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/store"
)

func TestRecoverKillsOrphanedGroup(t *testing.T) {
	q, st := setup(t, fakePipeline{run: func(jc *JobCtx) error { return nil }})
	cmd := exec.Command("sh", "-c", "trap '' INT; sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	pgid := cmd.Process.Pid
	j := submit(t, st)
	ctx := context.Background()
	_, _, _ = st.Transition(ctx, j.ID, store.Transition{To: api.StateArchiving, Mutate: func(j *store.Job) { j.Pgid = pgid }})
	if err := q.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if run.GroupAlive(pgid) {
		t.Fatal("orphaned group survived recovery")
	}
}
