package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/store"
)

func TestSimulatorHostCommandsRenewAndReleaseWithoutAdopting(t *testing.T) {
	const id = "0825B3D6-A17B-49A1-AC3C-0DADB895B9F8"
	cfg := &config.Server{DataDir: t.TempDir(), Simulators: config.SimulatorSettings{MaxLeaseDuration: config.Duration(45 * time.Minute)}}
	st, err := store.Open(filepath.Join(cfg.DataDir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	j, _, err := st.CreateJob(context.Background(), store.Job{App: "test", Verb: api.VerbCheck})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterSimulator(context.Background(), id, "eve-test", j.ID); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &out, JSON: true}
	run := func(args ...string) error {
		out.Reset()
		cmd := simulatorCmd(env, func() (*config.Server, error) { return cfg, nil })
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		cmd.SetArgs(args)
		return cmd.ExecuteContext(context.Background())
	}
	for _, args := range [][]string{{"renew", id}, {"renew", id, "--for", "0s"}, {"renew", id, "--for", "-1m"}, {"renew", id, "--for", "1h"}} {
		if err := run(args...); err == nil {
			t.Fatal("invalid/missing duration accepted", args)
		}
	}
	started := time.Now()
	if err := run("renew", strings.ToLower(id), "--for", "30m"); err != nil {
		t.Fatal(err)
	}
	var grant struct {
		UDID             string
		LeaseUntil       time.Time
		MaxLeaseDuration string
	}
	if err := json.Unmarshal(out.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.UDID != id || grant.MaxLeaseDuration != "45m0s" || grant.LeaseUntil.Before(started.Add(30*time.Minute-time.Second)) || grant.LeaseUntil.After(time.Now().Add(30*time.Minute)) {
		t.Fatal(out.String())
	}
	if err := run("list"); err != nil {
		t.Fatal(err)
	}
	var ds []simulatorView
	if err := json.Unmarshal(out.Bytes(), &ds); err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || !ds[0].LeaseActive || !ds[0].LeaseUntil.Equal(grant.LeaseUntil) {
		t.Fatal(out.String())
	}
	if err := run("release", id); err != nil {
		t.Fatal(err)
	}
	records, err := st.Simulators(context.Background())
	if err != nil || !records[0].LeaseUntil.IsZero() || !records[0].ReleasedAt.IsZero() {
		t.Fatal(records, err)
	}
	if err := run("renew", "2A8D25F8-B2D2-4E62-9CCE-A0A0C764B856", "--for", "30m"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("unknown device must not be adopted", err)
	}
	cfg.Simulators.ProtectedDevices = []string{id}
	if err := run("list"); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &ds); err != nil || !ds[0].ConfiguredProtected || ds[0].LeaseActive {
		t.Fatal("JSON omitted operator protection", out.String(), err)
	}
	// The previous indefinite commands must not provide an unbounded way around the cap.
	for _, verb := range []string{"protect", "unprotect"} {
		if err := run(verb, id); err == nil {
			t.Fatal("obsolete command still accepted", verb)
		}
	}
}
