// Package simulator manages only devices Eve created and durably registered before boot.
// Callers hold the Eve worker lock during Release/Sweep; a separate lifecycle lock serializes
// borrow-lease changes against shutdown. Other clients must renew before borrowing a device.
package simulator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/flock"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/self-deprecated/eve/internal/xcode"
)

type Manager struct {
	Store    *store.Store
	DataDir  string
	Settings config.SimulatorSettings
	Now      func() time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(m.DataDir, "simulators.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flock.Lock(f, false); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("simulator lifecycle busy: %w", err)
	}
	return func() { _ = flock.Unlock(f); _ = f.Close() }, nil
}

// Renew acquires or renews a finite borrow lease from now, never by adding to its old expiry.
// Excessive durations fail rather than silently granting less time than the caller requested.
func (m *Manager) Renew(ctx context.Context, id string, duration time.Duration) (time.Time, error) {
	if duration < time.Second {
		return time.Time{}, errors.New("lease duration must be at least 1s; use --for, e.g. --for 30m")
	}
	if duration > m.Settings.LeaseLimit() {
		return time.Time{}, fmt.Errorf("requested lease %s exceeds simulators.maxLeaseDuration (%s)", duration, m.Settings.LeaseLimit())
	}
	unlock, err := m.lock()
	if err != nil {
		return time.Time{}, err
	}
	defer unlock()
	until := m.now().Add(duration).UTC().Truncate(time.Millisecond)
	if err := m.Store.RenewSimulator(ctx, strings.ToUpper(id), until); err != nil {
		return time.Time{}, err
	}
	return until, nil
}

// ReleaseLease ends borrowing early. It never boots, shuts down or adopts a device, or
// overrides a running Eve job. Cleanup resumes under the usual idle and ownership checks.
func (m *Manager) ReleaseLease(ctx context.Context, id string) error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return m.Store.ReleaseSimulatorLease(ctx, strings.ToUpper(id))
}

// Release attempts teardown even after cancellation, under its own bounded context.
// It never masks the job outcome. A failed shutdown remains pending for Sweep.
func (m *Manager) Release(ctx context.Context, tools xcode.Tools, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.Settings.ShutdownWithin())
	defer cancel()
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.Store.ReleaseSimulator(ctx, id, m.now()); err != nil {
		return err
	}
	ds, err := m.Store.Simulators(ctx)
	if err != nil {
		return err
	}
	for _, d := range ds {
		if d.UDID == id {
			return m.shutdown(ctx, tools, d)
		}
	}
	return store.ErrNotFound
}

// Sweep retries released leases, and releases crash leases only after queue recovery has
// cleared their process group and requeued/finished the job. The caller holds the host lock,
// so neither a worker nor `eve run` can be actively using a device while this runs.
func (m *Manager) Sweep(ctx context.Context, tools xcode.Tools) error {
	idle := m.Settings.IdleAfter()
	if idle <= 0 {
		return nil
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	ds, err := m.Store.Simulators(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range ds {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		if d.Settled {
			continue
		}
		if d.ReleasedAt.IsZero() {
			j, err := m.Store.Get(ctx, d.JobID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if j.Pgid > 1 || (!j.State.Terminal() && j.State != api.StateQueued) {
				continue
			}
			// Start a fresh grace period on recovery; timestamps/CPU never expire live leases.
			if err := m.Store.ReleaseSimulator(ctx, d.UDID, m.now()); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if m.protected(d) {
			continue
		}
		if m.now().Sub(d.ReleasedAt) < idle {
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, m.Settings.ShutdownWithin())
		err := m.shutdown(attempt, tools, d)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.UDID, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) shutdown(ctx context.Context, tools xcode.Tools, d store.Simulator) (err error) {
	if d.Settled {
		return nil
	}
	// Cleanup must not overwrite the job's process group (including after a terminal result).
	tools.OnStart = nil
	defer func() {
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, m.Store.SimulatorCleanup(record, d.UDID, err == nil, detail, m.now()))
	}()
	if d.ReleasedAt.IsZero() {
		return errors.New("job still holds Simulator lease")
	}
	if m.protected(d) {
		if d.LeaseUntil.After(m.now()) {
			return fmt.Errorf("borrow lease valid until %s; shutdown deferred", d.LeaseUntil.Format(time.RFC3339Nano))
		}
		return errors.New("operator/legacy protection; shutdown deferred")
	}
	devices, err := tools.Simulators(ctx)
	if err != nil {
		return err
	}
	for _, actual := range devices {
		if !strings.EqualFold(actual.UDID, d.UDID) {
			continue
		}
		if actual.Name != d.Name {
			return errors.New("device renamed; ownership needs operator review")
		}
		switch actual.State {
		case "Shutdown":
			return nil
		case "Booted", "Booting":
			return tools.ShutdownSimulator(ctx, d.UDID)
		default:
			return fmt.Errorf("device state %q; shutdown deferred", actual.State)
		}
	}
	return errors.New("device missing from inventory; ownership needs operator review")
}

func (m *Manager) protected(d store.Simulator) bool {
	if d.LegacyProtected || d.LeaseUntil.After(m.now()) {
		return true
	}
	for _, id := range m.Settings.ProtectedDevices {
		if strings.EqualFold(id, d.UDID) {
			return true
		}
	}
	return false
}
