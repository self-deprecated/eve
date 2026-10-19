package simulator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/run"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/self-deprecated/eve/internal/xcode"
)

const ownedID = "0825B3D6-A17B-49A1-AC3C-0DADB895B9F8"

type rig struct {
	m              *Manager
	tools          xcode.Tools
	fake           *run.Fake
	job            store.Job
	now            time.Time
	device         xcode.SimDevice
	inventoryError bool
	shutdownError  bool
	shutdowns      []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	j, _, err := st.CreateJob(context.Background(), store.Job{App: "test", Verb: api.VerbCheck})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Transition(context.Background(), j.ID, store.Transition{To: api.StateTesting}); err != nil {
		t.Fatal(err)
	}
	r := &rig{job: j, now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), device: xcode.SimDevice{UDID: ownedID, Name: "eve-" + j.ID, State: "Booted"}}
	r.m = &Manager{Store: st, DataDir: dir, Now: func() time.Time { return r.now }}
	if err := st.RegisterSimulator(context.Background(), ownedID, r.device.Name, j.ID); err != nil {
		t.Fatal(err)
	}
	r.fake = &run.Fake{Handler: func(c run.Cmd) (string, int, error) {
		if c.Name != "/usr/bin/xcrun" || c.Args[0] != "simctl" {
			t.Fatalf("unexpected command: %s", c.String())
		}
		switch c.Args[1] {
		case "list":
			if r.inventoryError {
				return `{"devices":`, 0, nil
			}
			// Shared/legacy devices are deliberately present, including a misleading Eve name.
			ds := []xcode.SimDevice{
				r.device,
				{UDID: "2A8D25F8-B2D2-4E62-9CCE-A0A0C764B856", Name: "iPhone 17 Pro", State: "Booted"},
				{UDID: "4979D925-3782-4A2F-B08C-3381651EBD20", Name: "ScreenGate Screenshots", State: "Booted"},
				{UDID: "75302A75-37B5-41F5-8C51-D6A8F423869C", Name: "eve-not-owned", State: "Booted"},
			}
			b, _ := json.Marshal(map[string]any{"devices": map[string]any{"iOS-26-5": ds}})
			return string(b), 0, nil
		case "shutdown":
			r.shutdowns = append(r.shutdowns, c.Args[2])
			if r.shutdownError {
				return "CoreSimulator busy", 1, nil
			}
			r.device.State = "Shutdown"
			return "", 0, nil
		default:
			t.Fatalf("unsafe/unexpected command: %s", c.String())
		}
		return "", 0, nil
	}}
	r.tools = xcode.Tools{Runner: r.fake, OnStart: func(int) { t.Fatal("cleanup must not overwrite the job's process group") }}
	return r
}

func (r *rig) record(t *testing.T) store.Simulator {
	t.Helper()
	ds, err := r.m.Store.Simulators(context.Background())
	if err != nil || len(ds) != 1 {
		t.Fatalf("records: %+v %v", ds, err)
	}
	return ds[0]
}

func (r *rig) sweep(t *testing.T) {
	t.Helper()
	if err := r.m.Sweep(context.Background(), r.tools); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseAfterCancellationOnlyShutsDownOwnedDeviceOnce(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.m.Release(ctx, r.tools, ownedID); err != nil {
		t.Fatal(err)
	}
	if d := r.record(t); !d.Settled || !d.ReleasedAt.Equal(r.now) {
		t.Fatalf("%+v", d)
	}
	// A human/another client reboots it later; settled ownership is never a new cleanup request.
	r.device.State = "Booted"
	r.now = r.now.Add(24 * time.Hour)
	r.sweep(t)
	if !slices.Equal(r.shutdowns, []string{ownedID}) {
		t.Fatal(r.shutdowns)
	}
}

func TestQuietActiveLeaseNeverExpires(t *testing.T) {
	r := newRig(t)
	r.now = r.now.Add(365 * 24 * time.Hour)
	r.sweep(t)
	if !r.record(t).ReleasedAt.IsZero() || len(r.fake.Commands()) != 0 {
		t.Fatal("a quiet active job lost its lease")
	}
}

func TestRecoveryRequiresClearedJobAndStartsFreshIdlePeriod(t *testing.T) {
	for _, phase := range []api.State{api.StateQueued, api.StateSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			r := newRig(t)
			ctx := context.Background()
			if _, _, err := r.m.Store.Transition(ctx, r.job.ID, store.Transition{To: phase, Mutate: func(j *store.Job) { j.Pgid = 12345 }}); err != nil {
				t.Fatal(err)
			}
			r.now = r.now.Add(48 * time.Hour)
			r.sweep(t)
			if !r.record(t).ReleasedAt.IsZero() {
				t.Fatal("unrecovered process group lost its lease")
			}
			if _, err := r.m.Store.Update(ctx, r.job.ID, func(j *store.Job) { j.Pgid = 0 }); err != nil {
				t.Fatal(err)
			}
			r.sweep(t)
			if !r.record(t).ReleasedAt.Equal(r.now) || len(r.shutdowns) != 0 {
				t.Fatal("recovery must grant a new idle period")
			}
			r.now = r.now.Add(15*time.Minute - time.Millisecond)
			r.sweep(t)
			if len(r.shutdowns) != 0 {
				t.Fatal("shutdown before idle threshold")
			}
			r.now = r.now.Add(time.Millisecond)
			r.sweep(t)
			if !slices.Equal(r.shutdowns, []string{ownedID}) {
				t.Fatal(r.shutdowns)
			}
		})
	}
}

func TestFailedCleanupPersistsAndRetriesAfterIdle(t *testing.T) {
	r := newRig(t)
	r.shutdownError = true
	if err := r.m.Release(context.Background(), r.tools, ownedID); err == nil {
		t.Fatal("expected shutdown failure")
	}
	d := r.record(t)
	if d.Settled || d.ReleasedAt.IsZero() || !strings.Contains(d.LastError, "CoreSimulator busy") {
		t.Fatalf("%+v", d)
	}
	// Reopen the database as a new server would; no in-memory ownership is needed.
	if err := r.m.Store.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(r.m.DataDir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r.m.Store = st
	r.sweep(t)
	if len(r.shutdowns) != 1 {
		t.Fatal("retry before idle period")
	}
	r.now = r.now.Add(15 * time.Minute)
	r.shutdownError = false
	r.sweep(t)
	if len(r.shutdowns) != 2 || !r.record(t).Settled || r.record(t).LastError != "" {
		t.Fatalf("%v %+v", r.shutdowns, r.record(t))
	}
}

func TestBorrowLeaseSurvivesRestartAndExpiresAtDeadline(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	until, err := r.m.Renew(ctx, strings.ToLower(ownedID), 30*time.Minute)
	if err != nil || !until.Equal(r.now.Add(30*time.Minute)) {
		t.Fatal(until, err)
	}
	if err := r.m.Release(ctx, r.tools, ownedID); err == nil {
		t.Fatal("expected deferred job teardown")
	}
	r.now = r.now.Add(30*time.Minute - time.Millisecond)
	// Reopen as a new server; deadlines retain their original value, not a fresh TTL.
	if err := r.m.Store.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(r.m.DataDir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r.m = &Manager{Store: st, DataDir: r.m.DataDir, Now: func() time.Time { return r.now }}
	r.sweep(t)
	if len(r.fake.Commands()) != 0 || !r.record(t).LeaseUntil.Equal(until) {
		t.Fatal("borrow lease was lost/reset")
	}
	r.now = r.now.Add(time.Millisecond)
	r.sweep(t)
	if !r.record(t).Settled || len(r.shutdowns) != 1 {
		t.Fatal("expired borrow lease should permit normal cleanup")
	}
	if _, err := r.m.Renew(ctx, ownedID, time.Minute); !errors.Is(err, store.ErrSimulatorSettled) {
		t.Fatal("renew must not promise retention after cleanup settled", err)
	}
}

func TestRenewSetsFreshDeadlineWithoutStackingAndAppliesServerLimit(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	limit := config.Duration(40 * time.Minute)
	r.m.Settings.MaxLeaseDuration = limit
	first, err := r.m.Renew(ctx, ownedID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.m.Release(ctx, r.tools, ownedID); err == nil {
		t.Fatal("expected deferred job teardown")
	}
	r.now = r.now.Add(20 * time.Minute)
	until, err := r.m.Renew(ctx, ownedID, 40*time.Minute)
	if err != nil || !until.Equal(r.now.Add(40*time.Minute)) || !until.After(first) {
		t.Fatal(until, err)
	}
	for _, duration := range []time.Duration{0, -time.Minute, time.Millisecond, 40*time.Minute + time.Second} {
		if _, err := r.m.Renew(ctx, ownedID, duration); err == nil {
			t.Fatal("invalid lease duration accepted", duration)
		}
		if !r.record(t).LeaseUntil.Equal(until) {
			t.Fatal("rejected renewal altered the existing lease")
		}
	}
	// Repeated requests do not accumulate hours beyond the per-request cap.
	again, err := r.m.Renew(ctx, ownedID, 40*time.Minute)
	if err != nil || !again.Equal(until) {
		t.Fatal(again, err)
	}
	if _, err := r.m.Renew(ctx, "2A8D25F8-B2D2-4E62-9CCE-A0A0C764B856", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("must not adopt unknown devices", err)
	}
	r.now = first
	r.sweep(t)
	if len(r.shutdowns) != 0 {
		t.Fatal("renewed device shut down at its original expiry")
	}
	r.now = until
	r.sweep(t)
	if !r.record(t).Settled {
		t.Fatal("renewed lease did not expire at its new deadline")
	}
}

func TestReleaseAlreadyIdleBorrowedDeviceDoesNotAddGrace(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.m.Renew(ctx, ownedID, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Release(ctx, r.tools, ownedID); err == nil {
		t.Fatal("expected deferred job teardown")
	}
	r.now = r.now.Add(20 * time.Minute)
	if err := r.m.ReleaseLease(ctx, ownedID); err != nil {
		t.Fatal(err)
	}
	r.sweep(t)
	if !r.record(t).Settled {
		t.Fatal("already-idle device got another grace period after borrow release")
	}
}

func TestExpiredBorrowLeaseRetainsNormalIdleGrace(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.m.Renew(ctx, ownedID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Release(ctx, r.tools, ownedID); err == nil {
		t.Fatal("expected deferred job teardown")
	}
	r.now = r.now.Add(time.Minute)
	r.sweep(t)
	if len(r.shutdowns) != 0 {
		t.Fatal("borrow expiry bypassed the job's idle grace")
	}
	// A new agent can acquire again after expiry if cleanup has not already settled.
	until, err := r.m.Renew(ctx, ownedID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(15 * time.Minute)
	r.sweep(t)
	if len(r.shutdowns) != 0 {
		t.Fatal("new borrow lease after expiry was ignored")
	}
	r.now = until
	r.sweep(t)
	if !r.record(t).Settled {
		t.Fatal("new borrow lease did not expire")
	}
}

func TestReleaseBorrowLeaseEarlyPreservesJobAndIdleChecks(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.m.Renew(ctx, ownedID, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := r.m.ReleaseLease(ctx, ownedID); err != nil {
		t.Fatal(err)
	}
	r.sweep(t)
	if !r.record(t).ReleasedAt.IsZero() || len(r.fake.Commands()) != 0 {
		t.Fatal("borrow release must not expire the running job lease")
	}
	if _, err := r.m.Renew(ctx, ownedID, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Release(ctx, r.tools, ownedID); err == nil {
		t.Fatal("expected deferred job teardown")
	}
	released := r.now
	r.now = r.now.Add(5 * time.Minute)
	if err := r.m.ReleaseLease(ctx, ownedID); err != nil {
		t.Fatal(err)
	}
	r.sweep(t)
	if len(r.shutdowns) != 0 || !r.record(t).ReleasedAt.Equal(released) || !r.record(t).LeaseUntil.IsZero() {
		t.Fatal("borrow release must retain the normal job idle grace")
	}
	r.now = released.Add(15 * time.Minute)
	r.sweep(t)
	if !r.record(t).Settled {
		t.Fatal("early release did not restore normal cleanup")
	}
}

func TestBorrowExpiryNeverExpiresQuietJobLease(t *testing.T) {
	r := newRig(t)
	if _, err := r.m.Renew(context.Background(), ownedID, time.Minute); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(24 * time.Hour)
	r.sweep(t)
	if !r.record(t).ReleasedAt.IsZero() || len(r.fake.Commands()) != 0 {
		t.Fatal("borrow expiry interrupted the Eve job")
	}
}

func TestBorrowLeaseDoesNotDelayCrashRecoveryGrace(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.m.Renew(ctx, ownedID, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.m.Store.Transition(ctx, r.job.ID, store.Transition{To: api.StateQueued, Mutate: func(j *store.Job) { j.Pgid = 0 }}); err != nil {
		t.Fatal(err)
	}
	r.sweep(t)
	if !r.record(t).ReleasedAt.Equal(r.now) {
		t.Fatal("recovered job lease must release even during borrowing")
	}
	r.now = r.now.Add(30 * time.Minute)
	r.sweep(t)
	if !r.record(t).Settled {
		t.Fatal("lease expiry added another idle grace after crash recovery")
	}
}

func TestConfiguredProtectionAndDisabledIdleCleanup(t *testing.T) {
	r := newRig(t)
	r.m.Settings.ProtectedDevices = []string{strings.ToLower(ownedID)}
	if err := r.m.Release(context.Background(), r.tools, ownedID); err == nil {
		t.Fatal("expected configured protection")
	}
	zero := config.Duration(0)
	r.m.Settings = config.SimulatorSettings{IdleTimeout: &zero}
	r.now = r.now.Add(24 * time.Hour)
	r.sweep(t)
	if len(r.fake.Commands()) != 0 {
		t.Fatal("automatic cleanup must be disabled")
	}
	// Disabling the idle fallback does not disable explicit job teardown.
	if err := r.m.Release(context.Background(), r.tools, ownedID); err != nil {
		t.Fatal(err)
	}
	if len(r.shutdowns) != 1 {
		t.Fatal(r.shutdowns)
	}
}

func TestUncertainInventoryFailsClosed(t *testing.T) {
	for _, scenario := range []string{"renamed", "missing", "invalid JSON", "unknown state", "already stopped"} {
		t.Run(scenario, func(t *testing.T) {
			r := newRig(t)
			switch scenario {
			case "renamed":
				r.device.Name = "Human session"
			case "missing":
				r.device.UDID = "6CD831DA-23C4-47FA-A7C8-195406C19DC3"
			case "invalid JSON":
				r.inventoryError = true
			case "unknown state":
				r.device.State = "Creating"
			case "already stopped":
				r.device.State = "Shutdown"
			}
			err := r.m.Release(context.Background(), r.tools, ownedID)
			if scenario == "already stopped" {
				if err != nil || !r.record(t).Settled {
					t.Fatal(err)
				}
			} else if err == nil || r.record(t).Settled {
				t.Fatalf("must retain unresolved ownership: %v", err)
			}
			if len(r.shutdowns) != 0 {
				t.Fatal(r.shutdowns)
			}
		})
	}
}

type runnerFunc func(context.Context, run.Cmd) (run.Result, error)

func (f runnerFunc) Run(ctx context.Context, c run.Cmd) (run.Result, error) { return f(ctx, c) }

func TestCleanupDeadlineAndLeaseSerialization(t *testing.T) {
	r := newRig(t)
	r.m.Settings.ShutdownTimeout = config.Duration(10 * time.Millisecond)
	r.tools.Runner = runnerFunc(func(ctx context.Context, c run.Cmd) (run.Result, error) {
		if c.Args[1] != "shutdown" {
			return r.fake.Run(ctx, c)
		}
		if _, err := r.m.Renew(context.Background(), ownedID, time.Minute); err == nil {
			t.Fatal("renewal must not succeed while shutdown is in flight")
		}
		<-ctx.Done()
		return run.Result{}, ctx.Err()
	})
	started := time.Now()
	if err := r.m.Release(context.Background(), r.tools, ownedID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second || r.record(t).Settled {
		t.Fatal("shutdown was not bounded/pending")
	}
	if _, err := r.m.Renew(context.Background(), ownedID, time.Minute); err != nil {
		t.Fatal("cleanup lock leaked", err)
	}
}
