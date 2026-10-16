// Package queue runs eve's jobs: one worker at a time under a host-wide lock, parked jobs
// (processing, distributing) on watchers off the worker, cancellation, size-guard confirmation,
// and restart recovery that kills orphaned process groups before anything resumes
// (PLAN.md §4.1, §5.4–§5.6).
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/store"
)

// Pipeline executes one verb.
type Pipeline interface {
	// Run does the job's work on the worker. It must end the job (Finish or Park); returning an
	// error instead fails the job with class eve.internal.
	Run(jc *JobCtx) error
	// Watch continues a parked job (processing/distributing) off the worker. It must end the job
	// or return to let a later watcher retry.
	Watch(jc *JobCtx) error
}

// Router decides who is notified when a job enters a state (PLAN.md §8).
type Router func(j store.Job, s api.State) []store.NewDelivery

// Queue owns job execution.
type Queue struct {
	Store     *store.Store
	Hub       *Hub
	Pipelines map[api.Verb]Pipeline
	Route     Router
	DataDir   string
	Log       *slog.Logger
	// AfterCommit runs after every committed transition (e.g. kick the notification deliverer).
	AfterCommit func()
	// OrphanGrace is how long orphaned process groups get after SIGINT on recovery.
	OrphanGrace time.Duration

	kick     chan struct{}
	mu       sync.Mutex
	running  map[string]*running // job id → cancel
	watching map[string]bool
}

type running struct {
	cancel context.CancelFunc
	phase  func() api.State

	// mu orders Cancel against the end of a run: once settled, Cancel no longer relies on the
	// runner and records the cancellation itself.
	mu      sync.Mutex
	settled bool
	// byUser is set by Cancel. A context cancelled without it means the server is shutting
	// down: the job is left as it is, for restart recovery to resume (PLAN.md §5.5).
	byUser bool
}

// settle marks r finished and reports whether a user cancelled it.
func (r *running) settle() (byUser bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settled = true
	return r.byUser
}

// ErrNotCancellable is returned when cancelling would interrupt an upload in flight.
var ErrNotCancellable = errors.New("job is delivering to App Store Connect; cancel with force to interrupt it")

// New returns a queue; call Run to start it.
func New(st *store.Store, dataDir string, pipelines map[api.Verb]Pipeline, route Router, log *slog.Logger) *Queue {
	if log == nil {
		log = slog.Default()
	}
	return &Queue{
		Store: st, Hub: NewHub(), Pipelines: pipelines, Route: route, DataDir: dataDir, Log: log,
		OrphanGrace: 20 * time.Second,
		kick:        make(chan struct{}, 1), running: map[string]*running{}, watching: map[string]bool{},
	}
}

// Kick wakes the worker (after a submit or confirm).
func (q *Queue) Kick() {
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

// Run recovers unfinished jobs, then runs the worker until ctx ends.
func (q *Queue) Run(ctx context.Context) error {
	if err := q.Recover(ctx); err != nil {
		return fmt.Errorf("recovery: %w", err)
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		q.startWatchers(ctx)
		q.expireConfirmations(ctx)
		if err := q.runNext(ctx); err != nil && !errors.Is(err, store.ErrNotFound) {
			q.Log.Error("worker", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-q.kick:
		case <-tick.C:
		}
	}
}

// runNext runs the next queued job to completion, holding the host lock.
func (q *Queue) runNext(ctx context.Context) error {
	j, err := q.Store.NextQueued(ctx)
	if err != nil {
		return err
	}
	unlock, err := Lock(q.DataDir, false)
	if err != nil {
		q.Log.Info("worker waiting for host lock", "err", err)
		return nil
	}
	defer unlock()
	q.Exec(ctx, j)
	q.Kick() // look for the next job straight away
	return nil
}

// Exec runs one job synchronously on the calling goroutine (the worker, or `eve run`).
func (q *Queue) Exec(ctx context.Context, j store.Job) {
	p := q.Pipelines[j.Verb]
	jctx, cancel := context.WithCancel(ctx)
	jc := q.newJobCtx(jctx, j)
	q.mu.Lock()
	q.running[j.ID] = &running{cancel: cancel, phase: func() api.State { return jc.Job.State }}
	q.mu.Unlock()
	defer func() {
		cancel()
		q.mu.Lock()
		delete(q.running, j.ID)
		q.mu.Unlock()
	}()
	if p == nil {
		_ = jc.Fail("eve.unsupported", fmt.Sprintf("verb %s is not implemented yet", j.Verb), "", nil)
		return
	}
	err := p.Run(jc)
	q.settle(jc, err)
}

func (q *Queue) cancelledByUser(id string) bool {
	q.mu.Lock()
	r := q.running[id]
	q.mu.Unlock()
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byUser
}

func (q *Queue) runner(id string) *running {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running[id]
}

// settle makes sure a job that returned from its pipeline is terminal or parked.
func (q *Queue) settle(jc *JobCtx, err error) {
	byUser := false
	if r := q.runner(jc.Job.ID); r != nil {
		byUser = r.settle()
	}
	cur, gerr := q.Store.Get(context.Background(), jc.Job.ID)
	if gerr != nil {
		q.Log.Error("settle", "job", jc.Job.ID, "err", gerr)
		return
	}
	if cur.State.Terminal() || cur.State.Parked() || cur.State == api.StateWaitingForDevice {
		return
	}
	switch {
	case errors.Is(jc.Ctx.Err(), context.Canceled) && !byUser:
		// Shutdown, not a cancel: leave the job for Recover on the next start.
		q.Log.Info("leaving job for restart recovery", "job", jc.Job.ID, "state", cur.State)
	case errors.Is(jc.Ctx.Err(), context.Canceled):
		_ = jc.finishDetached(api.StateCancelled, "cancelled", nil)
	case err != nil:
		_ = jc.failDetached("eve.internal", err.Error())
	default:
		_ = jc.failDetached("eve.internal", "pipeline returned without finishing the job")
	}
}

// startWatchers starts a watcher for every parked processing/distributing job not yet watched.
func (q *Queue) startWatchers(ctx context.Context) {
	for _, s := range []api.State{api.StateProcessing, api.StateDistributing} {
		jobs, err := q.Store.List(ctx, store.Filter{State: s, Limit: 100})
		if err != nil {
			q.Log.Error("list parked", "err", err)
			continue
		}
		for _, j := range jobs {
			q.mu.Lock()
			if q.watching[j.ID] {
				q.mu.Unlock()
				continue
			}
			q.watching[j.ID] = true
			q.mu.Unlock()
			go q.watch(ctx, j)
		}
	}
}

func (q *Queue) watch(ctx context.Context, j store.Job) {
	defer func() {
		q.mu.Lock()
		delete(q.watching, j.ID)
		q.mu.Unlock()
	}()
	p := q.Pipelines[j.Verb]
	if p == nil {
		return
	}
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jc := q.newJobCtx(jctx, j)
	q.mu.Lock()
	q.running[j.ID] = &running{cancel: cancel, phase: func() api.State { return jc.Job.State }}
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.running, j.ID)
		q.mu.Unlock()
	}()
	if err := p.Watch(jc); err != nil {
		q.Log.Warn("watcher", "job", j.ID, "err", err)
	}
	if r := q.runner(j.ID); r != nil && r.settle() {
		if cur, err := q.Store.Get(context.Background(), j.ID); err == nil && !cur.State.Terminal() {
			_, _ = q.finish(context.Background(), j.ID, api.StateCancelled, "cancelled", nil)
		}
	}
}

// expireConfirmations cancels size-guard pauses nobody answered (PLAN.md §6.7.3).
func (q *Queue) expireConfirmations(ctx context.Context) {
	jobs, err := q.Store.List(ctx, store.Filter{State: api.StateAwaitingConfirmation, Limit: 100})
	if err != nil {
		return
	}
	for _, j := range jobs {
		if !j.DeadlineAt.IsZero() && time.Now().After(j.DeadlineAt) {
			jc := q.newJobCtx(ctx, j)
			_ = jc.Fail("size.confirmation_timeout", "Nobody confirmed or rejected the size increase in time.",
				"Resubmit, then `eve confirm` or fix the project.", func(j *store.Job) {
					if j.Result.Size != nil {
						j.Result.Size.Gate = api.GateFailed
					}
				}, api.StateCancelled)
		}
	}
}

// Confirm accepts a size-guard pause; the job is requeued at the front to continue delivery.
func (q *Queue) Confirm(ctx context.Context, id, reason string) (store.Job, error) {
	if reason == "" {
		return store.Job{}, errors.New("a reason is required: say what the growth is")
	}
	j, err := q.Store.Get(ctx, id)
	if err != nil {
		return store.Job{}, err
	}
	if j.State != api.StateAwaitingConfirmation {
		return j, fmt.Errorf("job %s is %s, not awaiting confirmation", id, j.State)
	}
	j, err = q.transition(ctx, id, store.Transition{
		To: api.StateQueued, Message: "size increase confirmed: " + reason,
		Mutate: func(j *store.Job) {
			j.ResumeFrom, j.Confirmation, j.Priority, j.DeadlineAt = api.StateDelivering, reason, 10, time.Time{}
			if j.Result.Size != nil {
				j.Result.Size.Gate, j.Result.Size.Confirmation = api.GateConfirmed, reason
			}
		},
	})
	q.Kick()
	return j, err
}

// Reject refuses a size-guard pause; the job ends cancelled.
func (q *Queue) Reject(ctx context.Context, id string) (store.Job, error) {
	j, err := q.Store.Get(ctx, id)
	if err != nil {
		return store.Job{}, err
	}
	if j.State != api.StateAwaitingConfirmation {
		return j, fmt.Errorf("job %s is %s, not awaiting confirmation", id, j.State)
	}
	return q.finish(ctx, id, api.StateCancelled, "size increase rejected", func(j *store.Job) {
		j.Result.Failure = &api.Failure{Class: "size.rejected", Summary: "The size increase was rejected; nothing was uploaded.", By: "rules"}
		if j.Result.Size != nil {
			j.Result.Size.Gate = api.GateFailed
		}
	})
}

// Cancel stops a job. A job in delivering is refused unless force is set (PLAN.md §11).
func (q *Queue) Cancel(ctx context.Context, id string, force bool) (store.Job, error) {
	j, err := q.Store.Get(ctx, id)
	if err != nil {
		return store.Job{}, err
	}
	if j.State.Terminal() {
		return j, nil
	}
	if r := q.runner(id); r != nil {
		r.mu.Lock()
		if !r.settled {
			if r.phase() == api.StateDelivering && !force {
				r.mu.Unlock()
				return j, ErrNotCancellable
			}
			r.byUser = true
			r.mu.Unlock()
			r.cancel()
			// The worker or watcher records the cancellation when it returns.
			return j, nil
		}
		r.mu.Unlock()
	}
	// Queued, parked without a watcher, or its run already ended: record it here.
	return q.finish(ctx, id, api.StateCancelled, "cancelled", nil)
}

// Recover resolves jobs left unfinished by a previous run: kill orphaned process groups first,
// then requeue or resume each job by phase (PLAN.md §5.5).
func (q *Queue) Recover(ctx context.Context) error {
	jobs, err := q.Store.Unfinished(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Pgid > 1 && run.GroupAlive(j.Pgid) && ownedByUs(j.Pgid) {
			q.Log.Warn("killing orphaned process group", "job", j.ID, "pgid", j.Pgid)
			run.KillGroup(j.Pgid, q.OrphanGrace)
		}
	}
	for _, j := range jobs {
		switch j.State {
		case api.StateQueued, api.StateAwaitingConfirmation, api.StateProcessing, api.StateDistributing:
			// Queued jobs wait for the worker, pauses keep their deadline, watchers resume parked jobs.
			continue
		}
		resume := j.State
		if j.ResumeFrom == resume && j.PhaseAttempts >= 1 {
			_, _ = q.finish(ctx, j.ID, api.StateInterrupted, fmt.Sprintf("interrupted twice in %s", resume), func(j *store.Job) {
				j.Result.Failure = &api.Failure{Class: "eve.interrupted", Summary: fmt.Sprintf("The server stopped twice while this job was %s.", resume), By: "rules"}
			})
			continue
		}
		if _, err := q.transition(ctx, j.ID, store.Transition{
			To: api.StateQueued, Message: fmt.Sprintf("server restarted during %s; resuming", resume),
			Mutate: func(j *store.Job) {
				j.ResumeFrom, j.Priority, j.Pgid = resume, 10, 0
				j.PhaseAttempts++
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

// ownedByUs checks the process group leader still belongs to this user (pgids get reused).
func ownedByUs(pgid int) bool {
	var st syscall.Stat_t
	if err := syscall.Stat(fmt.Sprintf("/proc/%d", pgid), &st); err == nil {
		return int(st.Uid) == os.Getuid()
	}
	// macOS has no /proc: kill(-pgid, 0) only succeeds for groups we may signal, i.e. ours.
	return syscall.Kill(-pgid, 0) == nil
}

func (q *Queue) transition(ctx context.Context, id string, t store.Transition) (store.Job, error) {
	if t.Deliveries == nil && q.Route != nil && t.To != "" {
		to := t.To
		t.Deliveries = func(j store.Job) []store.NewDelivery { return q.Route(j, to) }
	}
	j, ev, err := q.Store.Transition(ctx, id, t)
	if err != nil {
		return j, err
	}
	q.Hub.Publish(ev)
	if q.AfterCommit != nil {
		q.AfterCommit()
	}
	return j, nil
}

func (q *Queue) finish(ctx context.Context, id string, to api.State, msg string, mutate func(*store.Job)) (store.Job, error) {
	return q.transition(ctx, id, store.Transition{To: to, Message: msg, Mutate: mutate})
}

// Lock takes the host-wide build lock shared by the server's worker and `eve run`. With wait
// false it fails immediately when the lock is held.
func Lock(dataDir string, wait bool) (func(), error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "eve.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another eve build holds %s: %w", f.Name(), err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
