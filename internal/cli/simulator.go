package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/simulator"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/spf13/cobra"
)

type simulatorView struct {
	store.Simulator
	ConfiguredProtected bool `json:"configuredProtected,omitempty"`
	LeaseActive         bool `json:"leaseActive"`
}

// These host-local commands never adopt devices, run simctl, or load signing secrets.
func simulatorCmd(env *Env, load func() (*config.Server, error)) *cobra.Command {
	root := &cobra.Command{Use: "simulator", Short: "Inspect and renew borrow leases for Eve-owned Simulators on the build host"}
	for _, action := range []string{"list", "renew", "release"} {
		cmd := &cobra.Command{Use: action, Args: cobra.NoArgs}
		var duration time.Duration
		switch action {
		case "list":
			cmd.Short = "List owned devices, borrow lease expiry and pending cleanup"
		case "renew":
			cmd.Short = "Acquire or renew a borrow lease for a concrete duration"
			cmd.Flags().DurationVar(&duration, "for", 0, "lease duration from now, e.g. 30m (required; capped by server config)")
			_ = cmd.MarkFlagRequired("for")
		case "release":
			cmd.Short = "End a borrow lease early; normal job and idle checks still apply"
		}
		if action != "list" {
			cmd.Use += " <udid>"
			cmd.Args = cobra.ExactArgs(1)
		}
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			st, err := store.Open(filepath.Join(cfg.DataDir, "eve.db"))
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			m := simulator.Manager{Store: st, DataDir: cfg.DataDir, Settings: cfg.Simulators}
			switch action {
			case "renew":
				until, err := m.Renew(cmd.Context(), args[0], duration)
				if err != nil {
					return err
				}
				id := strings.ToUpper(args[0])
				if env.JSON {
					return env.printJSON(struct {
						UDID             string    `json:"udid"`
						LeaseUntil       time.Time `json:"leaseUntil"`
						MaxLeaseDuration string    `json:"maxLeaseDuration"`
					}{id, until, cfg.Simulators.LeaseLimit().String()})
				}
				_, err = fmt.Fprintf(env.Out, "%s leased until %s (maximum per renewal: %s)\n", id, until.Format(time.RFC3339Nano), cfg.Simulators.LeaseLimit())
				return err
			case "release":
				if err := m.ReleaseLease(cmd.Context(), args[0]); err != nil {
					return err
				}
				id := strings.ToUpper(args[0])
				if env.JSON {
					return env.printJSON(struct {
						UDID     string `json:"udid"`
						Released bool   `json:"released"`
					}{id, true})
				}
				_, err := fmt.Fprintf(env.Out, "%s borrow lease released; normal job and idle checks apply\n", id)
				return err
			}
			ds, err := st.Simulators(cmd.Context())
			if err != nil {
				return err
			}
			now := time.Now()
			views := make([]simulatorView, 0, len(ds))
			for _, d := range ds {
				v := simulatorView{Simulator: d, LeaseActive: d.LeaseUntil.After(now)}
				for _, id := range cfg.Simulators.ProtectedDevices {
					v.ConfiguredProtected = v.ConfiguredProtected || strings.EqualFold(id, d.UDID)
				}
				views = append(views, v)
			}
			if env.JSON {
				return env.printJSON(views)
			}
			for _, v := range views {
				state := "job-leased"
				if !v.ReleasedAt.IsZero() {
					state = "pending"
				}
				if v.Settled {
					state = "settled"
				}
				if v.LegacyProtected {
					state += ",legacy-protected"
				}
				if v.ConfiguredProtected {
					state += ",operator-protected"
				}
				if !v.LeaseUntil.IsZero() {
					if v.LeaseActive {
						state += ",borrowed-until="
					} else {
						state += ",lease-expired="
					}
					state += v.LeaseUntil.Format(time.RFC3339Nano)
				}
				if _, err := fmt.Fprintf(env.Out, "%s  %s  %s  %s  %s\n", v.UDID, v.Name, v.JobID, state, v.LastError); err != nil {
					return err
				}
			}
			return nil
		}
		root.AddCommand(cmd)
	}
	return root
}
