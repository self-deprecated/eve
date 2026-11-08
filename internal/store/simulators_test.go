package store

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSimulatorLeaseMigrationPreservesLegacyPinsForExplicitConversion(t *testing.T) {
	for _, convert := range []string{"renew", "release"} {
		t.Run(convert, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "eve.db")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			ctx := context.Background()
			j, _, err := st.CreateJob(ctx, newJob("driver", "abc", "key"))
			if err != nil {
				t.Fatal(err)
			}
			const id = "0825B3D6-A17B-49A1-AC3C-0DADB895B9F8"
			if err := st.RegisterSimulator(ctx, id, "eve-test", j.ID); err != nil {
				t.Fatal(err)
			}
			// Reconstruct the schema before lease_until and an existing indefinite pin.
			rewindSimulatorSchema(t, st, "lease_until", `UPDATE simulators SET protected=1`)
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			ds, err := st.Simulators(ctx)
			if err != nil || len(ds) != 1 || !ds[0].LegacyProtected || !ds[0].LeaseUntil.IsZero() {
				t.Fatal("migration silently removed an existing pin", ds, err)
			}
			until := time.Date(2026, 10, 19, 14, 0, 0, 0, time.UTC)
			if convert == "renew" {
				err = st.RenewSimulator(ctx, id, until)
			} else {
				err = st.ReleaseSimulatorLease(ctx, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			ds, err = st.Simulators(ctx)
			if err != nil || ds[0].LegacyProtected || !ds[0].ReleasedAt.IsZero() {
				t.Fatal(ds, err)
			}
			if convert == "renew" && !ds[0].LeaseUntil.Equal(until) {
				t.Fatal(ds)
			}
			if convert == "release" && !ds[0].LeaseUntil.IsZero() {
				t.Fatal(ds)
			}
		})
	}
}

// rewindSimulatorSchema drops column and every simulators column added after it, then sets the
// schema version to just before column's migration, so the next Open replays them.
func rewindSimulatorSchema(t *testing.T, st *Store, column string, setup ...string) {
	t.Helper()
	ctx := context.Background()
	added := []string{"lease_until", "settled_at", "deleted_at"} // in migration order
	from := slices.Index(added, column)
	at := slices.IndexFunc(migrations, func(m string) bool {
		return strings.HasPrefix(m, "ALTER TABLE simulators ADD COLUMN "+column+" ")
	})
	if from < 0 || at < 0 {
		t.Fatalf("no simulators migration adds %s", column)
	}
	stmts := setup
	for _, c := range added[from:] {
		stmts = append(stmts, "ALTER TABLE simulators DROP COLUMN "+c)
	}
	stmts = append(stmts, fmt.Sprintf("UPDATE schema_version SET v=%d", at))
	for _, stmt := range stmts {
		if _, err := st.db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(stmt, err)
		}
	}
}

func TestSettledAtMigrationStartsGraceAtTheSuccessfulShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eve.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	j, _, err := st.CreateJob(ctx, newJob("driver", "abc", "key"))
	if err != nil {
		t.Fatal(err)
	}
	const settled, pending = "0825B3D6-A17B-49A1-AC3C-0DADB895B9F8", "2A8D25F8-B2D2-4E62-9CCE-A0A0C764B856"
	shutdownAt := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	for _, id := range []string{settled, pending} {
		if err := st.RegisterSimulator(ctx, id, "eve-"+id[:4], j.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Records written by the previous release: one settled, one with a failed attempt.
	rewindSimulatorSchema(t, st, "settled_at",
		fmt.Sprintf(`UPDATE simulators SET settled=1,last_attempt_at=%d WHERE udid='%s'`, shutdownAt.UnixMilli(), settled),
		fmt.Sprintf(`UPDATE simulators SET last_attempt_at=%d,last_error='busy' WHERE udid='%s'`, shutdownAt.UnixMilli(), pending))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	ds, err := st.Simulators(ctx)
	if err != nil || len(ds) != 2 {
		t.Fatal(ds, err)
	}
	for _, d := range ds {
		want := time.Time{}
		if d.UDID == settled {
			want = shutdownAt
		}
		if !d.SettledAt.Equal(want) || !d.DeletedAt.IsZero() {
			t.Fatalf("%s: settledAt %v deletedAt %v, want settledAt %v", d.UDID, d.SettledAt, d.DeletedAt, want)
		}
	}
}

func TestSimulatorCleanupAndDeletionTimestamps(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	j, _, err := st.CreateJob(ctx, newJob("driver", "abc", "key"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "0825B3D6-A17B-49A1-AC3C-0DADB895B9F8"
	if err := st.RegisterSimulator(ctx, id, "eve-test", j.ID); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	get := func() Simulator {
		t.Helper()
		ds, err := st.Simulators(ctx)
		if err != nil || len(ds) != 1 {
			t.Fatal(ds, err)
		}
		return ds[0]
	}
	steps := []func() error{
		func() error { return st.SimulatorCleanup(ctx, id, false, "busy", t0) },
		func() error { return st.SimulatorCleanup(ctx, id, true, "", t0.Add(time.Minute)) },
		// A repeated success never moves the start of the grace period.
		func() error { return st.SimulatorCleanup(ctx, id, true, "", t0.Add(time.Hour)) },
		func() error {
			return st.SimulatorDeletion(ctx, id, false, "booted after cleanup", t0.Add(13*time.Hour))
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if d := get(); !d.SettledAt.Equal(t0.Add(time.Minute)) || !d.DeletedAt.IsZero() || d.LastError != "booted after cleanup" {
		t.Fatalf("%+v", d)
	}
	// An unchanged reason is not rewritten every sweep.
	if err := st.SimulatorDeletion(ctx, id, false, "booted after cleanup", t0.Add(14*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if d := get(); !d.LastAttemptAt.Equal(t0.Add(13 * time.Hour)) {
		t.Fatalf("%+v", d)
	}
	for _, at := range []time.Time{t0.Add(15 * time.Hour), t0.Add(16 * time.Hour)} {
		if err := st.SimulatorDeletion(ctx, id, true, "", at); err != nil {
			t.Fatal(err)
		}
	}
	if d := get(); !d.DeletedAt.Equal(t0.Add(15*time.Hour)) || d.LastError != "" {
		t.Fatalf("deletion must be recorded once: %+v", d)
	}
}
