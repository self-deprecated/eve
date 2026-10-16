package queue

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/store"
)

type fakePipeline struct {
	run   func(jc *JobCtx) error
	watch func(jc *JobCtx) error
}

func (f fakePipeline) Run(jc *JobCtx) error { return f.run(jc) }
func (f fakePipeline) Watch(jc *JobCtx) error {
	if f.watch == nil {
		return nil
	}
	return f.watch(jc)
}

func setup(t *testing.T, p Pipeline) (*Queue, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q := New(st, dir, map[api.Verb]Pipeline{api.VerbTestflight: p}, nil, nil)
	q.OrphanGrace = 300 * time.Millisecond
	return q, st
}

func submit(t *testing.T, st *store.Store) store.Job {
	t.Helper()
	j, _, err := st.CreateJob(context.Background(), store.Job{App: "driver", Verb: api.VerbTestflight, Request: api.JobRequest{App: "driver"}})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func get(t *testing.T, st *store.Store, id string) store.Job {
	t.Helper()
	j, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestExecOutcomes(t *testing.T) {
	cases := map[string]struct {
		run   func(jc *JobCtx) error
		state api.State
		class string
	}{
		"success": {func(jc *JobCtx) error { _ = jc.Enter(api.StateArchiving, ""); return jc.Succeed("ok", nil) }, api.StateSucceeded, ""},
		"error":   {func(jc *JobCtx) error { return errors.New("boom") }, api.StateFailed, "eve.internal"},
		"silent":  {func(jc *JobCtx) error { return nil }, api.StateFailed, "eve.internal"},
		"human":   {func(jc *JobCtx) error { return jc.NeedsHuman(api.HumanUnlockPhone, "locked", "", nil) }, api.StateNeedsHuman, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			q, st := setup(t, fakePipeline{run: c.run})
			j := submit(t, st)
			q.Exec(context.Background(), j)
			got := get(t, st, j.ID)
			if got.State != c.state {
				t.Fatalf("state %s, want %s", got.State, c.state)
			}
			if c.class != "" && (got.Result.Failure == nil || got.Result.Failure.Class != c.class) {
				t.Fatalf("failure %+v", got.Result.Failure)
			}
		})
	}
}

func TestCancelRunningAndDeliveringGuard(t *testing.T) {
	entered := make(chan struct{})
	q, st := setup(t, fakePipeline{run: func(jc *JobCtx) error {
		_ = jc.Enter(api.StateDelivering, "uploading")
		close(entered)
		<-jc.Ctx.Done()
		return jc.Ctx.Err()
	}})
	j := submit(t, st)
	done := make(chan struct{})
	go func() { q.Exec(context.Background(), j); close(done) }()
	<-entered
	if _, err := q.Cancel(context.Background(), j.ID, false); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancel during delivering must need force, got %v", err)
	}
	if _, err := q.Cancel(context.Background(), j.ID, true); err != nil {
		t.Fatal(err)
	}
	<-done
	if s := get(t, st, j.ID).State; s != api.StateCancelled {
		t.Fatal(s)
	}
}

func TestCancelQueued(t *testing.T) {
	q, st := setup(t, fakePipeline{run: func(jc *JobCtx) error { return nil }})
	j := submit(t, st)
	if _, err := q.Cancel(context.Background(), j.ID, false); err != nil {
		t.Fatal(err)
	}
	if s := get(t, st, j.ID).State; s != api.StateCancelled {
		t.Fatal(s)
	}
}

func TestRecoverRequeuesThenInterrupts(t *testing.T) {
	q, st := setup(t, fakePipeline{run: func(jc *JobCtx) error { return nil }})
	j := submit(t, st)
	ctx := context.Background()
	_, _, _ = st.Transition(ctx, j.ID, store.Transition{To: api.StateArchiving})
	if err := q.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got := get(t, st, j.ID)
	if got.State != api.StateQueued || got.ResumeFrom != api.StateArchiving || got.Priority == 0 {
		t.Fatalf("not requeued for resume: %+v", got)
	}
	// Second crash in the same phase.
	_, _, _ = st.Transition(ctx, j.ID, store.Transition{To: api.StateArchiving, Mutate: func(j *store.Job) { j.PhaseAttempts = 1 }})
	if err := q.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if s := get(t, st, j.ID).State; s != api.StateInterrupted {
		t.Fatalf("second interruption: %s", s)
	}
}

func TestRecoverKillsOrphanedGroup(t *testing.T) {
	q, st := setup(t, fakePipeline{run: func(jc *JobCtx) error { return nil }})
	cmd := exec.Command("/bin/sh", "-c", "trap '' INT; sleep 30")
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

func TestRecoverLeavesParkedJobs(t *testing.T) {
	q, st := setup(t, fakePipeline{run: func(jc *JobCtx) error { return nil }})
	ctx := context.Background()
	a, b := submit(t, st), submit(t, st)
	_, _, _ = st.Transition(ctx, a.ID, store.Transition{To: api.StateAwaitingConfirmation})
	_, _, _ = st.Transition(ctx, b.ID, store.Transition{To: api.StateProcessing})
	_ = q.Recover(ctx)
	if get(t, st, a.ID).State != api.StateAwaitingConfirmation || get(t, st, b.ID).State != api.StateProcessing {
		t.Fatal("parked jobs must stay parked")
	}
}

func TestConfirmRejectAndExpiry(t *testing.T) {
	q, st := setup(t, fakePipeline{run: func(jc *JobCtx) error { return nil }})
	ctx := context.Background()
	a, b, c := submit(t, st), submit(t, st), submit(t, st)
	for _, j := range []store.Job{a, b, c} {
		id := j.ID
		_, _, _ = st.Transition(ctx, id, store.Transition{To: api.StateAwaitingConfirmation, Mutate: func(j *store.Job) {
			j.DeadlineAt = time.Now().Add(time.Hour)
			j.Result.Size = &api.SizeReport{Gate: api.GateAwaitingConfirmation}
		}})
	}
	if _, err := q.Confirm(ctx, a.ID, ""); err == nil {
		t.Fatal("confirm without a reason accepted")
	}
	got, err := q.Confirm(ctx, a.ID, "added onboarding videos")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != api.StateQueued || got.ResumeFrom != api.StateDelivering || got.Result.Size.Gate != api.GateConfirmed || got.Confirmation == "" {
		t.Fatalf("%+v", got)
	}
	if _, err := q.Reject(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if s := get(t, st, b.ID); s.State != api.StateCancelled || s.Result.Failure.Class != "size.rejected" || s.Result.Size.Gate != api.GateFailed {
		t.Fatalf("%+v", s)
	}
	_, _ = st.Update(ctx, c.ID, func(j *store.Job) { j.DeadlineAt = time.Now().Add(-time.Minute) })
	q.expireConfirmations(ctx)
	if s := get(t, st, c.ID); s.State != api.StateCancelled || s.Result.Failure.Class != "size.confirmation_timeout" {
		t.Fatalf("%+v", s)
	}
}

func TestWatcherRunsParkedJobs(t *testing.T) {
	watched := make(chan string, 1)
	q, st := setup(t, fakePipeline{
		run:   func(jc *JobCtx) error { return nil },
		watch: func(jc *JobCtx) error { watched <- jc.Job.ID; return jc.Succeed("processed", nil) },
	})
	ctx := context.Background()
	j := submit(t, st)
	_, _, _ = st.Transition(ctx, j.ID, store.Transition{To: api.StateProcessing})
	q.startWatchers(ctx)
	select {
	case id := <-watched:
		if id != j.ID {
			t.Fatal(id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not run")
	}
	time.Sleep(50 * time.Millisecond)
	if s := get(t, st, j.ID).State; s != api.StateSucceeded {
		t.Fatal(s)
	}
}

func TestHubDeliversCommittedEvents(t *testing.T) {
	q, st := setup(t, fakePipeline{run: func(jc *JobCtx) error { return jc.Succeed("ok", nil) }})
	j := submit(t, st)
	ch, stop := q.Hub.Subscribe(j.ID)
	defer stop()
	q.Exec(context.Background(), j)
	select {
	case ev := <-ch:
		if ev.State != api.StateSucceeded {
			t.Fatal(ev.State)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir, false); err == nil {
		t.Fatal("second lock acquired")
	}
	unlock()
	u2, err := Lock(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	u2()
}

func TestRouterCalledOnTransitions(t *testing.T) {
	var seen []api.State
	q, st := setup(t, fakePipeline{run: func(jc *JobCtx) error { _ = jc.Enter(api.StateArchiving, ""); return jc.Succeed("ok", nil) }})
	q.Route = func(j store.Job, s api.State) []store.NewDelivery {
		seen = append(seen, s)
		if s == api.StateSucceeded {
			return []store.NewDelivery{{Event: api.NotifySucceeded, TargetKind: "t3", TargetAddress: "thread", Payload: api.Notification{ID: j.ID, Result: j.Result}}}
		}
		return nil
	}
	j := submit(t, st)
	q.Exec(context.Background(), j)
	rows, _ := st.OutboxForJob(context.Background(), j.ID)
	if len(rows) != 1 || rows[0].Payload.Result.Outcome != api.StateSucceeded {
		t.Fatalf("outbox %+v (states %v)", rows, seen)
	}
}
