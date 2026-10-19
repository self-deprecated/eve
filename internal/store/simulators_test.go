package store

import (
	"context"
	"path/filepath"
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
			// Reconstruct the previous schema and an existing indefinite pin.
			for _, stmt := range []string{`UPDATE simulators SET protected=1`, `ALTER TABLE simulators DROP COLUMN lease_until`} {
				if _, err := st.db.ExecContext(ctx, stmt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE schema_version SET v=?`, len(migrations)-1); err != nil {
				t.Fatal(err)
			}
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
