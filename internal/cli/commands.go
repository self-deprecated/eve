package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/client"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/ledger"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func versionCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version (and the server's, if reachable)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := map[string]string{"client": env.Version}
			if c, err := env.client(); err == nil {
				ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
				defer cancel()
				if v, err := c.Version(ctx); err == nil {
					out["server"] = v.Version
				}
			}
			if env.JSON {
				return env.printJSON(out)
			}
			_, _ = fmt.Fprintf(env.Out, "eve %s", out["client"])
			if s, ok := out["server"]; ok {
				_, _ = fmt.Fprintf(env.Out, " (server %s)", s)
			}
			_, _ = fmt.Fprintln(env.Out)
			return nil
		},
	}
}

func loginCmd(env *Env) *cobra.Command {
	var token string
	cmd := &cobra.Command{
		Use:   "login <server-url>",
		Short: "Store the server URL and a token (create one on the build Mac with `eve token create <name>`)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			u, err := url.Parse(args[0])
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return &ExitError{Code: api.ExitUsage, Err: fmt.Errorf("server URL must look like https://eve.<tailnet>.ts.net")}
			}
			if token == "" {
				_, _ = fmt.Fprint(env.Err, "token: ")
				line, err := bufio.NewReader(os.Stdin).ReadString('\n')
				if err != nil && line == "" {
					return err
				}
				token = strings.TrimSpace(line)
			}
			if token == "" {
				return &ExitError{Code: api.ExitUsage, Err: errors.New("no token given")}
			}
			c := client.New(args[0], token)
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			v, err := c.Version(ctx)
			if err != nil {
				return fmt.Errorf("could not log in to %s: %w", args[0], err)
			}
			dir := config.ClientDir()
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			b, _ := yaml.Marshal(config.Client{Server: strings.TrimRight(args[0], "/")})
			if err := os.WriteFile(filepath.Join(dir, "client.yml"), b, 0o600); err != nil {
				return err
			}
			if err := os.WriteFile(client.TokenPath(), []byte(token+"\n"), 0o600); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(env.Out, "logged in to %s (server %s)\n", args[0], v.Version)
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "token (read from stdin when omitted)")
	return cmd
}

func doctorCmd(env *Env) *cobra.Command {
	var host bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the connection to the server; --host also runs the server's host checks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			v, err := c.Version(ctx)
			if err != nil {
				return err
			}
			switch {
			case v.API < api.APILevel:
				_, _ = fmt.Fprintf(env.Err, "warning: the server is older than this client (API level %d, needs %d); update eve on the build Mac\n", v.API, api.APILevel)
			case v.API > api.APILevel:
				_, _ = fmt.Fprintf(env.Err, "warning: this client is older than the server (API level %d, server %d); update it (sd fast-lane update)\n", api.APILevel, v.API)
			}
			if !host {
				if env.JSON {
					return env.printJSON(map[string]any{"server": c.BaseURL, "serverVersion": v.Version, "serverAPI": v.API, "status": "ok"})
				}
				_, _ = fmt.Fprintf(env.Out, "ok: %s (server %s, API level %d)\n", c.BaseURL, v.Version, v.API)
				return nil
			}
			raw, err := c.Raw(ctx, "/v1/doctor")
			if err != nil {
				return err
			}
			if env.JSON {
				_, err = env.Out.Write(append(raw, '\n'))
				return err
			}
			_, _ = fmt.Fprint(env.Out, FormatDoctor(raw))
			return nil
		},
	}
	cmd.Flags().BoolVar(&host, "host", false, "run the server's host checks (keychain, Xcode, App Store Connect, disk)")
	return cmd
}

func waitCmd(env *Env) *cobra.Command {
	var w waitFlags
	cmd := &cobra.Command{
		Use:   "wait <job>",
		Short: "Follow a job until it finishes or needs confirmation; the exit code is the outcome",
		Long: "Holds one connection to the server and reconnects through network drops and server\n" +
			"restarts without losing events. Run it as a long-running (background) command: waiting\n" +
			"costs nothing. Exit codes: 0 succeeded, 1 failed, 65 rejected, 69 unavailable,\n" +
			"75 still running at --timeout, 76 confirmation required, 78 needs a human, 130 cancelled.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			return follow(cmd.Context(), env, c, args[0], w)
		},
	}
	w.register(cmd)
	return cmd
}

func statusCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "status [job]",
		Short: "Show a job's result, or the latest jobs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				return listJobs(cmd.Context(), env, c, "", 10)
			}
			r, err := c.Job(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if err := env.writeResult(r); err != nil {
				return err
			}
			_ = ledger.Default().Collect(c.BaseURL, r.ID, r.Outcome)
			if d, err := c.Deliveries(cmd.Context(), args[0]); err == nil && !env.JSON {
				_, _ = fmt.Fprint(env.Out, FormatDeliveries(d))
			}
			return nil
		},
	}
}

func listJobs(ctx context.Context, env *Env, c *client.Client, app string, limit int) error {
	jobs, err := c.Jobs(ctx, app, limit)
	if err != nil {
		return err
	}
	if env.JSON {
		return env.printJSON(jobs)
	}
	_, _ = fmt.Fprint(env.Out, FormatJobs(jobs))
	return nil
}

func jobsCmd(env *Env) *cobra.Command {
	var app string
	var limit int
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List recent jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			return listJobs(cmd.Context(), env, c, app, limit)
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "only this app")
	cmd.Flags().IntVar(&limit, "limit", 20, "how many")
	return cmd
}

func logsCmd(env *Env) *cobra.Command {
	var phase string
	var errorsOnly bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <job>",
		Short: "Print a job's log for a phase (default: the phase it ended in)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			if phase == "" {
				r, err := c.Job(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				phase = string(r.Phase)
				phases, _ := c.Logs(cmd.Context(), args[0])
				if !contains(phases, phase) && len(phases) > 0 {
					phase = phases[len(phases)-1]
				}
				if len(phases) == 0 {
					return fmt.Errorf("job %s has no logs yet", args[0])
				}
			}
			s, err := c.Log(cmd.Context(), args[0], phase, errorsOnly, tail)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprint(env.Out, s)
			return nil
		},
	}
	cmd.Flags().StringVar(&phase, "phase", "", "phase log to print (archiving, delivering, …)")
	cmd.Flags().BoolVar(&errorsOnly, "errors", false, "only the relevant error lines")
	cmd.Flags().IntVar(&tail, "tail", 0, "only the last N lines")
	return cmd
}

func cancelCmd(env *Env) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "cancel <job>",
		Short: "Cancel a job (a job uploading to App Store Connect needs --force)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			r, err := c.Cancel(cmd.Context(), args[0], force)
			if err != nil {
				return err
			}
			if env.JSON {
				return env.printJSON(r)
			}
			_, _ = fmt.Fprintf(env.Out, "cancel requested for %s (now %s)\n", r.ID, r.Outcome)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "interrupt an upload in flight")
	return cmd
}

func confirmCmd(env *Env) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "confirm <job> --reason TEXT",
		Short: "Accept a size-guard pause: the build continues to App Store Connect",
		Long: "Confirm only if every large or new path in `eve size <job>` is something the change meant to ship.\n" +
			"Repository files in the app (.swift, docs/, project.yml) are never expected: use `eve reject`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(reason) == "" {
				return &ExitError{Code: api.ExitUsage, Err: errors.New("--reason is required: name what grew and why")}
			}
			c, err := env.client()
			if err != nil {
				return err
			}
			r, err := c.Confirm(cmd.Context(), args[0], reason)
			if err != nil {
				return err
			}
			if env.JSON {
				return env.printJSON(r)
			}
			_, _ = fmt.Fprintf(env.Out, "confirmed %s; delivery continues. You will be notified when it finishes.\n", r.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "what grew and why (stored with the job)")
	return cmd
}

func rejectCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "reject <job>",
		Short: "Refuse a size-guard pause; the job ends cancelled and nothing is uploaded",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			r, err := c.Reject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if env.JSON {
				return env.printJSON(r)
			}
			_, _ = fmt.Fprintf(env.Out, "rejected %s; fix the project and resubmit.\n", r.ID)
			return nil
		},
	}
}

func sizeCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "size <job>",
		Short: "Show the size report and the full diff against the app's baseline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			raw, err := c.Size(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if env.JSON {
				_, err = env.Out.Write(append(raw, '\n'))
				return err
			}
			_, _ = fmt.Fprint(env.Out, FormatSize(raw))
			return nil
		},
	}
}

func buildsCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "builds <app>",
		Short: "Show the app's latest App Store Connect builds and their states",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return rawGet(cmd.Context(), env, "/v1/apps/"+url.PathEscape(args[0])+"/builds")
		},
	}
}

func appsCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "apps",
		Short: "List the apps the server is configured for",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return rawGet(cmd.Context(), env, "/v1/apps")
		},
	}
}

func baselineCmd(env *Env) *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "baseline <app> [--from <job>]",
		Short: "Show the app's size baseline, or reset it from a job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/v1/apps/" + url.PathEscape(args[0]) + "/baseline"
			if from == "" {
				return rawGet(cmd.Context(), env, path)
			}
			c, err := env.client()
			if err != nil {
				return err
			}
			raw, err := c.PutJSON(cmd.Context(), path, map[string]string{"from": from})
			if err != nil {
				return err
			}
			_, err = env.Out.Write(append(raw, '\n'))
			return err
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "job whose bundle becomes the baseline")
	return cmd
}

func rawGet(ctx context.Context, env *Env, path string) error {
	c, err := env.client()
	if err != nil {
		return err
	}
	raw, err := c.Raw(ctx, path)
	if err != nil {
		return err
	}
	_, err = env.Out.Write(append(raw, '\n'))
	return err
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
