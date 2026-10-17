package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/asc"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/launchd"
	"github.com/self-deprecated/eve/internal/notify"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/spf13/cobra"
)

// DefaultServerConfig is server.yml's default location.
func DefaultServerConfig() string { return filepath.Join(config.ClientDir(), "server.yml") }

// AddServerCommands adds the commands that run on the build host.
func AddServerCommands(root *cobra.Command, env *Env) {
	var cfgPath string
	root.PersistentFlags().StringVar(&cfgPath, "config", DefaultServerConfig(), "server.yml (server commands)")
	load := func() (*config.Server, error) { return config.LoadServer(cfgPath) }
	root.AddCommand(serveCmd(env, load), runCmd(env, load), tokenCmd(env, load), agentCmd(env, &cfgPath))
}

func serveCmd(env *Env, load func() (*config.Server, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the build server (normally started by launchd in the GUI session)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			if !launchd.InAquaSession() {
				log.Warn("not running in the GUI (Aqua) session: signing with the Xcode account and the login keychain will fail")
			}
			sys, err := Assemble(cmd.Context(), cfg, env.Version, log)
			if err != nil {
				return err
			}
			defer sys.Close()
			if err := sys.EnsureOwnerToken(cmd.Context()); err != nil {
				return err
			}

			ln, err := net.Listen("tcp", cfg.Listen)
			if err != nil {
				return err
			}
			httpSrv := &http.Server{Handler: sys.Server.Handler(), ReadHeaderTimeout: 10 * time.Second}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			errc := make(chan error, 3)
			queueDone := make(chan struct{})
			go func() { errc <- sys.Queue.Run(ctx); close(queueDone) }()
			go func() { errc <- sys.Deliverer.Run(ctx) }()
			go func() {
				if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					errc <- err
				}
			}()
			go sys.Housekeep(ctx)
			log.Info("eve serving", "listen", cfg.Listen, "data", cfg.DataDir, "version", env.Version)
			select {
			case <-ctx.Done():
			case err = <-errc:
			}
			cancel()
			shutdown, c2 := context.WithTimeout(context.Background(), 10*time.Second)
			defer c2()
			_ = httpSrv.Shutdown(shutdown)
			// Let the worker stop its build's process group (SIGINT, then SIGKILL after 20 s);
			// the job itself stays where it was for restart recovery.
			select {
			case <-queueDone:
			case <-time.After(25 * time.Second):
				log.Warn("worker did not stop in time; restart recovery will kill any leftover build")
			}
			return err
		},
	}
}

// runCmd runs one job in-process on the build host, without a server: development and
// break-glass. It takes the same host lock as the server's worker.
func runCmd(env *Env, load func() (*config.Server, error)) *cobra.Command {
	var f submitFlags
	cmd := &cobra.Command{
		Use:   "run <testflight|device|check|exec> [path] [-- command for exec]",
		Short: "Run a job in-process on this machine (no server), following it to the end",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var command []string
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				args, command = args[:dash], args[dash:]
			}
			if len(args) < 1 || len(args) > 2 {
				return &ExitError{Code: api.ExitUsage, Err: errors.New("usage: eve run <verb> [path] [-- command for exec]")}
			}
			cfg, err := load()
			if err != nil {
				return err
			}
			verb := api.Verb(args[0])
			if !verb.Valid() {
				return &ExitError{Code: api.ExitUsage, Err: fmt.Errorf("unknown verb %q", args[0])}
			}
			if (verb == api.VerbExec) != (len(command) > 0) {
				return &ExitError{Code: api.ExitUsage, Err: errors.New("exec, and only exec, takes a command after --")}
			}
			dir := "."
			if len(args) == 2 {
				dir = args[1]
			}
			root, err := config.FindRepoRoot(dir)
			if err != nil {
				return &ExitError{Code: api.ExitUsage, Err: err}
			}
			repo, err := config.LoadRepo(root)
			if err != nil {
				return &ExitError{Code: api.ExitUsage, Err: err}
			}
			log := slog.New(slog.NewTextHandler(env.Err, &slog.HandlerOptions{Level: slog.LevelWarn}))
			sys, err := Assemble(cmd.Context(), cfg, env.Version, log)
			if err != nil {
				return err
			}
			defer sys.Close()
			unlock, err := queue.Lock(cfg.DataDir, false)
			if err != nil {
				return &ExitError{Code: api.ExitUnavailable, Err: err}
			}
			defer unlock()
			j, _, err := sys.Store.CreateJob(cmd.Context(), store.Job{
				App: pick(f.app, repo.App), Verb: verb, TokenName: "eve-run",
				Request: api.JobRequest{
					App: pick(f.app, repo.App), Verb: verb, Source: api.JobSource{Path: root},
					Options: api.JobOptions{
						BuildNumber: f.buildNumber, Notes: f.notes, Device: f.device, Release: f.release,
						ExpectSizeChange: f.expectSize, Command: command,
					},
				},
			})
			if err != nil {
				return err
			}
			ch, stop := sys.Queue.Hub.Subscribe(j.ID)
			defer stop()
			go func() {
				for ev := range ch {
					if ev.Message != "" && !env.JSON {
						_, _ = fmt.Fprintf(env.Err, "%s  %-22s %s\n", ev.Time.Local().Format("15:04:05"), ev.State, ev.Message)
					}
				}
			}()
			_, _ = fmt.Fprintf(env.Err, "job %s\n", j.ID)
			sys.Queue.Exec(cmd.Context(), j)
			j, _ = sys.Store.Get(context.Background(), j.ID)
			// Follow parked work (processing, a size-guard pause is reported, not waited on).
			for j.State == api.StateProcessing || j.State == api.StateDistributing {
				p := sys.Queue.Pipelines[j.Verb]
				if err := p.Watch(sys.Queue.NewJobCtx(cmd.Context(), j)); err != nil {
					return err
				}
				if cmd.Context().Err() != nil {
					break
				}
				j, _ = sys.Store.Get(context.Background(), j.ID)
			}
			return env.printResult(j.Result)
		},
	}
	cmd.Flags().StringVar(&f.app, "app", "", "app name (default: from eve.yml)")
	cmd.Flags().StringVar(&f.buildNumber, "build-number", "", "explicit build number")
	cmd.Flags().StringVar(&f.notes, "notes", "", "What to Test text")
	cmd.Flags().StringVar(&f.device, "device", "", "phone for device: name or UDID")
	cmd.Flags().BoolVar(&f.release, "release", false, "Release configuration for device")
	cmd.Flags().StringVar(&f.expectSize, "expect-size-change", "", "pre-approve bundle growth, with a reason")
	return cmd
}

func tokenCmd(env *Env, load func() (*config.Server, error)) *cobra.Command {
	openStore := func() (*store.Store, error) {
		cfg, err := load()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return nil, err
		}
		return store.Open(filepath.Join(cfg.DataDir, "eve.db"))
	}
	cmd := &cobra.Command{Use: "token", Short: "Manage API tokens (on the build host)"}
	cmd.AddCommand(&cobra.Command{
		Use: "create <name>", Short: "Create (or replace) a named token and print it once", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			tok, err := st.CreateToken(cmd.Context(), args[0], false)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(env.Out, tok)
			_, _ = fmt.Fprintf(env.Err, "on the other machine: eve login <server-url> --token %s…\n", tok[:12])
			return nil
		},
	}, &cobra.Command{
		Use: "list", Short: "List tokens", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			ts, err := st.ListTokens(cmd.Context())
			if err != nil {
				return err
			}
			if env.JSON {
				return env.printJSON(ts)
			}
			for _, t := range ts {
				state := "active"
				if t.Revoked {
					state = "revoked"
				}
				used := "never"
				if !t.LastUsedAt.IsZero() {
					used = t.LastUsedAt.Local().Format(time.DateTime)
				}
				_, _ = fmt.Fprintf(env.Out, "%-16s %-8s owner=%-5v last used %s\n", t.Name, state, t.Owner, used)
			}
			return nil
		},
	}, &cobra.Command{
		Use: "revoke <name>", Short: "Revoke a token", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			return st.RevokeToken(cmd.Context(), args[0])
		},
	})
	return cmd
}

func agentCmd(env *Env, cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Manage the LaunchAgent on machines without the Nix module"}
	cmd.AddCommand(&cobra.Command{
		Use: "install", Short: "Install `eve serve` as a LaunchAgent in this user's GUI session", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadServer(*cfgPath)
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			exe, _ = filepath.EvalSymlinks(exe)
			abs, _ := filepath.Abs(*cfgPath)
			if err := launchd.Install(launchd.Spec{Program: exe, Config: abs, LogDir: filepath.Join(cfg.DataDir, "logs")}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(env.Out, "installed %s\n", launchd.Path())
			return nil
		},
	}, &cobra.Command{
		Use: "uninstall", Short: "Remove the LaunchAgent", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return launchd.Uninstall() },
	}, &cobra.Command{
		Use: "status", Short: "Show the LaunchAgent's state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := launchd.Status()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(env.Out, s)
			return nil
		},
	})
	return cmd
}

// ascFromConfig returns an App Store Connect client, or nil when no key is configured.
func ascFromConfig(cfg *config.Server) (*asc.Client, error) {
	if cfg.ASC.KeyID == "" {
		return nil, nil
	}
	return asc.New(cfg.ASC)
}

var _ notify.Outbox = outboxAdapter{}
