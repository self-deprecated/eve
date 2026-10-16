package queue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/store"
)

// JobCtx is what a pipeline sees of its job. Store writes use a background context so that a
// cancelled job can still record that it was cancelled.
type JobCtx struct {
	Ctx context.Context
	Job store.Job
	Dir string // <dataDir>/jobs/<id>
	q   *Queue
}

func (q *Queue) newJobCtx(ctx context.Context, j store.Job) *JobCtx {
	return &JobCtx{Ctx: ctx, Job: j, Dir: filepath.Join(q.DataDir, "jobs", j.ID), q: q}
}

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// errShuttingDown refuses terminal transitions while the server stops: whatever failed was
// most likely interrupted by the shutdown itself, and restart recovery resumes the job.
var errShuttingDown = errors.New("server shutting down; the job is left for restart recovery")

func (jc *JobCtx) apply(t store.Transition) error {
	if t.To.Terminal() && jc.Ctx.Err() != nil && !jc.q.cancelledByUser(jc.Job.ID) {
		return errShuttingDown
	}
	ctx, cancel := bg()
	defer cancel()
	j, err := jc.q.transition(ctx, jc.Job.ID, t)
	if err == nil {
		jc.Job = j
	}
	return err
}

// Enter moves the job to a running phase.
func (jc *JobCtx) Enter(s api.State, msg string) error {
	return jc.apply(store.Transition{To: s, Message: msg})
}

// EnterWith moves the job to a running phase and changes fields atomically.
func (jc *JobCtx) EnterWith(s api.State, msg string, mutate func(*store.Job)) error {
	return jc.apply(store.Transition{To: s, Message: msg, Mutate: mutate})
}

// Note records an informational event.
func (jc *JobCtx) Note(msg string) {
	ctx, cancel := bg()
	defer cancel()
	if ev, err := jc.q.Store.Message(ctx, jc.Job.ID, msg); err == nil {
		jc.q.Hub.Publish(ev)
	}
}

// Update changes job fields without an event.
func (jc *JobCtx) Update(mutate func(*store.Job)) error {
	ctx, cancel := bg()
	defer cancel()
	j, err := jc.q.Store.Update(ctx, jc.Job.ID, mutate)
	if err == nil {
		jc.Job = j
	}
	return err
}

// OnStart records a command's process group so restart recovery can kill it.
func (jc *JobCtx) OnStart(pgid int) {
	_ = jc.Update(func(j *store.Job) { j.Pgid = pgid })
}

// Succeed ends the job successfully.
func (jc *JobCtx) Succeed(msg string, mutate func(*store.Job)) error {
	return jc.apply(store.Transition{To: api.StateSucceeded, Message: msg, Mutate: func(j *store.Job) {
		j.Pgid = 0
		if mutate != nil {
			mutate(j)
		}
	}})
}

// Fail ends the job with a classified failure. The outcome defaults to failed; pass another
// terminal state (rejected, unavailable, cancelled) to override.
func (jc *JobCtx) Fail(class, summary, hint string, mutate func(*store.Job), to ...api.State) error {
	return jc.FailWith(&api.Failure{Class: class, Summary: summary, Hint: hint, By: "rules"}, mutate, to...)
}

// FailWith ends the job with a prepared failure (e.g. from the classifier).
func (jc *JobCtx) FailWith(f *api.Failure, mutate func(*store.Job), to ...api.State) error {
	state := api.StateFailed
	if len(to) > 0 {
		state = to[0]
	}
	return jc.apply(store.Transition{To: state, Message: f.Summary, Mutate: func(j *store.Job) {
		j.Pgid = 0
		j.Result.Failure = f
		if mutate != nil {
			mutate(j)
		}
	}})
}

// NeedsHuman ends the job because a person must act.
func (jc *JobCtx) NeedsHuman(action, detail, url string, mutate func(*store.Job)) error {
	return jc.apply(store.Transition{To: api.StateNeedsHuman, Message: action + ": " + detail, Mutate: func(j *store.Job) {
		j.Pgid = 0
		j.Result.Human = &api.Human{Action: action, Detail: detail, URL: url}
		if mutate != nil {
			mutate(j)
		}
	}})
}

// Park moves the job off the worker into a waiting state with a deadline.
func (jc *JobCtx) Park(s api.State, deadline time.Time, msg string, mutate func(*store.Job)) error {
	return jc.apply(store.Transition{To: s, Message: msg, Mutate: func(j *store.Job) {
		j.Pgid, j.DeadlineAt = 0, deadline
		if mutate != nil {
			mutate(j)
		}
	}})
}

// LogPath returns the log file for a phase, creating the directory.
func (jc *JobCtx) LogPath(phase string) string {
	dir := filepath.Join(jc.Dir, "logs")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, phase+".log")
}

func (jc *JobCtx) finishDetached(s api.State, msg string, mutate func(*store.Job)) error {
	return jc.apply(store.Transition{To: s, Message: msg, Mutate: mutate})
}

func (jc *JobCtx) failDetached(class, summary string) error {
	return jc.Fail(class, summary, "", nil)
}

// NewJobCtx returns a job context outside the worker (watch loops in `eve run`, tests).
func (q *Queue) NewJobCtx(ctx context.Context, j store.Job) *JobCtx { return q.newJobCtx(ctx, j) }
