package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newJob(app, sha, key string) Job {
	return Job{
		App: app, Verb: api.VerbTestflight, SourceSHA: sha, IdempotencyKey: key,
		Request: api.JobRequest{App: app, Verb: api.VerbTestflight},
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eve.db")
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}

func TestIdempotencyExplicitKey(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, existing, err := s.CreateJob(ctx, newJob("driver", "abc", "k1"))
	if err != nil || existing {
		t.Fatal(err, existing)
	}
	b, existing, err := s.CreateJob(ctx, newJob("driver", "zzz", "k1"))
	if err != nil || !existing || b.ID != a.ID {
		t.Fatalf("same key must return the same job: %v %v %s %s", err, existing, a.ID, b.ID)
	}
}

func TestImplicitDedupOnlyAgainstLiveJobs(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, _, _ := s.CreateJob(ctx, newJob("driver", "abc", ""))
	b, existing, _ := s.CreateJob(ctx, newJob("driver", "abc", ""))
	if !existing || b.ID != a.ID {
		t.Fatal("live duplicate not deduplicated")
	}
	if _, _, err := s.Transition(ctx, a.ID, Transition{To: api.StateNeedsHuman}); err != nil {
		t.Fatal(err)
	}
	c, existing, _ := s.CreateJob(ctx, newJob("driver", "abc", ""))
	if existing || c.ID == a.ID {
		t.Fatal("a resubmit after a terminal job must start a new job")
	}
}

func TestTransitionWritesEventAndOutboxAtomically(t *testing.T) {
	s, ctx := open(t), context.Background()
	j, _, _ := s.CreateJob(ctx, newJob("driver", "abc", ""))
	if _, _, err := s.Transition(ctx, j.ID, Transition{To: api.StateBuilding, Message: "building"}); err != nil {
		t.Fatal(err)
	}
	done, ev, err := s.Transition(ctx, j.ID, Transition{
		To: api.StateSucceeded, Message: "done",
		Mutate: func(j *Job) { j.BuildNumber = "202610161512"; j.Result.Build = &api.BuildInfo{Number: "202610161512"} },
		Deliveries: func(j Job) []NewDelivery {
			return []NewDelivery{
				{Event: api.NotifySucceeded, TargetKind: "t3", TargetAddress: "thread-1", Payload: api.Notification{Event: api.NotifySucceeded, ID: j.ID, Result: j.Result}},
				{Event: api.NotifySucceeded, TargetKind: "webhook", TargetAddress: "https://x.ts.net/h", Payload: api.Notification{Event: api.NotifySucceeded, ID: j.ID, Result: j.Result}},
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.Result.Outcome != api.StateSucceeded || done.Phase != api.StateBuilding || done.FinishedAt.IsZero() {
		t.Fatalf("result/phase not maintained: %+v", done)
	}
	rows, _ := s.OutboxDue(ctx, time.Now().Add(time.Second), 10)
	if len(rows) != 2 || rows[0].Payload.Result.Build.Number != "202610161512" || rows[0].Payload.DeliveryID == "" {
		t.Fatalf("outbox rows: %+v", rows)
	}
	if rows[0].Payload.DeliveryID == rows[1].Payload.DeliveryID {
		t.Fatal("delivery ids must differ per target")
	}
	evs, _ := s.Events(ctx, j.ID, 0)
	if len(evs) != 3 || evs[2].Seq != ev.Seq {
		t.Fatalf("events: %+v", evs)
	}
	if _, _, err := s.Transition(ctx, j.ID, Transition{To: api.StateFailed}); err == nil {
		t.Fatal("transition out of a terminal state accepted")
	}
}

func TestOutboxLifecycle(t *testing.T) {
	s, ctx := open(t), context.Background()
	j, _, _ := s.CreateJob(ctx, newJob("driver", "abc", ""))
	_, _, _ = s.Transition(ctx, j.ID, Transition{To: api.StateFailed, Deliveries: func(j Job) []NewDelivery {
		return []NewDelivery{{Event: api.NotifyFailed, TargetKind: "t3", TargetAddress: "t", Payload: api.Notification{ID: j.ID}}}
	}})
	now := time.Now()
	rows, _ := s.OutboxDue(ctx, now.Add(time.Second), 10)
	if err := s.OutboxRetry(ctx, rows[0].ID, 1, now.Add(time.Minute), "refused"); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.OutboxDue(ctx, now.Add(time.Second), 10); len(due) != 0 {
		t.Fatal("retry not deferred")
	}
	if due, _ := s.OutboxDue(ctx, now.Add(2*time.Minute), 10); len(due) != 1 || due[0].LastError != "refused" {
		t.Fatal("retry not due later")
	}
	_ = s.OutboxExhausted(ctx, rows[0].ID, "gave up")
	all, _ := s.OutboxForJob(ctx, j.ID)
	if all[0].Status != "exhausted" {
		t.Fatal(all[0].Status)
	}
}

func TestAllocateBuildNumber(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.SetClock(func() time.Time { return time.Date(2026, 10, 16, 15, 12, 30, 0, time.UTC) })
	n1, _ := s.AllocateBuildNumber(ctx, "driver", "", "")
	n2, _ := s.AllocateBuildNumber(ctx, "driver", "", "")
	if n1 != "202610161512" || n2 != "202610161513" {
		t.Fatalf("same-minute allocations must stay unique: %s %s", n1, n2)
	}
	// App Store Connect already has a higher build (uploaded outside eve).
	n3, _ := s.AllocateBuildNumber(ctx, "driver", "202610161600", "")
	if n3 != "202610161600" {
		t.Fatalf("ASC floor ignored: %s", n3)
	}
	// Lineage's old builds were "1" and "2": the timestamp wins.
	l, _ := s.AllocateBuildNumber(ctx, "lineage", "3", "")
	if l != "202610161512" {
		t.Fatal(l)
	}
	if _, err := s.AllocateBuildNumber(ctx, "driver", "", "5"); err == nil {
		t.Fatal("a requested number below the last one was accepted")
	}
	if n, err := s.AllocateBuildNumber(ctx, "driver", "", "202701010000"); err != nil || n != "202701010000" {
		t.Fatal(n, err)
	}
}

func TestAllocateConcurrentIsUnique(t *testing.T) {
	s, ctx := open(t), context.Background()
	seen := sync.Map{}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := s.AllocateBuildNumber(ctx, "driver", "", "")
			if err != nil {
				t.Error(err)
				return
			}
			if _, dup := seen.LoadOrStore(n, true); dup {
				t.Errorf("duplicate build number %s", n)
			}
		}()
	}
	wg.Wait()
}

func TestTokens(t *testing.T) {
	s, ctx := open(t), context.Background()
	tok, err := s.CreateToken(ctx, "lattice", false)
	if err != nil {
		t.Fatal(err)
	}
	ti, err := s.VerifyToken(ctx, tok)
	if err != nil || ti.Name != "lattice" || ti.Owner {
		t.Fatal(ti, err)
	}
	if _, err := s.VerifyToken(ctx, tok+"x"); err != ErrNotFound {
		t.Fatal("wrong token accepted")
	}
	_ = s.RevokeToken(ctx, "lattice")
	if _, err := s.VerifyToken(ctx, tok); err != ErrNotFound {
		t.Fatal("revoked token accepted")
	}
}

func TestUnfinishedAndNextQueued(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, _, _ := s.CreateJob(ctx, newJob("a", "1", ""))
	time.Sleep(2 * time.Millisecond)
	b, _, _ := s.CreateJob(ctx, newJob("b", "2", ""))
	_, _ = s.Update(ctx, b.ID, func(j *Job) { j.Priority = 1 })
	next, _ := s.NextQueued(ctx)
	if next.ID != b.ID {
		t.Fatal("priority not honoured")
	}
	_, _, _ = s.Transition(ctx, a.ID, Transition{To: api.StateArchiving})
	un, _ := s.Unfinished(ctx)
	if len(un) != 2 {
		t.Fatal(len(un))
	}
}
