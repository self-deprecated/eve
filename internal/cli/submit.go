package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/client"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/ledger"
	"github.com/self-deprecated/eve/internal/sizeguard"
	"github.com/self-deprecated/eve/internal/source"
	"github.com/self-deprecated/eve/internal/store"
	"github.com/spf13/cobra"
)

type submitFlags struct {
	app, key, buildNumber, notes, device, path string
	acceptSource, expectSize                   string
	notifyURLs                                 []string
	release, wait                              bool
	follow                                     waitFlags
}

func submitCmd(env *Env) *cobra.Command {
	var f submitFlags
	cmd := &cobra.Command{
		Use:   "submit <testflight|check|device|exec> [-- command for exec]",
		Short: "Send the working tree to Quartz and start a job; returns at once (or waits with --wait)",
		Long: "Uploads the repository (git-tracked and untracked-but-not-ignored files; never build output or\n" +
			"secrets) after the size guard's source check, then queues the job. With --wait it then follows\n" +
			"the job until it finishes or needs confirmation; the exit code is the outcome.\n\n" +
			"testflight uploads to TestFlight; device installs and launches on the paired iPhone (if the\n" +
			"phone is locked or asleep, it retries for up to 10 minutes until it's unlocked); check builds for the\n" +
			"Simulator (and tests with check.test); exec runs a repo command on Quartz with $EVE_ARTIFACTS\n" +
			"set (fetch them with `eve artifacts <job>`). `eve apps` lists what the server allows for each\n" +
			"app (exec needs allowExec); submit refuses anything else with exit 64 before uploading.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runSubmit(cmd.Context(), env, f, args) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.app, "app", "", "app name (default: from eve.yml)")
	fl.StringArrayVar(&f.notifyURLs, "notify-url", nil, "webhook to POST the result to (repeatable; must be allow-listed on the server)")
	fl.StringVar(&f.key, "idempotency-key", "", "retry key: the same key returns the same job")
	fl.StringVar(&f.buildNumber, "build-number", "", "explicit build number (default: allocated by eve)")
	fl.StringVar(&f.notes, "notes", "", "\"What to Test\" text for TestFlight")
	fl.StringVar(&f.device, "device", "", "phone for `device`: name or UDID (default: the server's, else the paired iPhone)")
	fl.BoolVar(&f.release, "release", false, "Release configuration for `device`")
	fl.StringVar(&f.acceptSource, "accept-source-size", "", "upload despite the source check, with a reason")
	fl.StringVar(&f.expectSize, "expect-size-change", "", "pre-approve app bundle growth for this job, with a reason")
	fl.StringVar(&f.path, "path", "", "build this checkout in place on Quartz instead of uploading (owner token only)")
	fl.BoolVar(&f.wait, "wait", false, "then follow the job like `eve wait`; the exit code is the outcome")
	f.follow.register(cmd)
	return cmd
}

func runSubmit(ctx context.Context, env *Env, f submitFlags, args []string) error {
	verb := api.Verb(args[0])
	if !verb.Valid() {
		return &ExitError{Code: api.ExitUsage, Err: fmt.Errorf("unknown verb %q: want testflight, check, device or exec", args[0])}
	}
	c, err := env.client()
	if err != nil {
		return err
	}
	req := api.JobRequest{
		Verb: verb, NotifyURLs: f.notifyURLs,
		Options: api.JobOptions{BuildNumber: f.buildNumber, Notes: f.notes, Device: f.device, Release: f.release, ExpectSizeChange: f.expectSize},
	}
	if verb == api.VerbExec {
		req.Options.Command = args[1:]
		if len(req.Options.Command) == 0 {
			return &ExitError{Code: api.ExitUsage, Err: errors.New("exec needs a command: eve submit exec -- scripts/screenshots.sh")}
		}
	}
	req.IdempotencyKey = f.key
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = store.NewKey()
	}
	lg := ledger.Default()

	var root string
	if f.path != "" {
		root, err = filepath.Abs(f.path)
	} else {
		root, err = config.FindRepoRoot(".")
	}
	if err != nil {
		return &ExitError{Code: api.ExitUsage, Err: err}
	}
	repo, err := config.LoadRepo(root)
	if err != nil {
		return &ExitError{Code: api.ExitUsage, Err: err}
	}
	req.App = pick(f.app, repo.App)
	if err := c.CheckServer(ctx); err != nil {
		return &ExitError{Code: api.ExitUnavailable, Err: fmt.Errorf("%w; nothing was uploaded", err)}
	}
	if err := checkVerb(ctx, c, req.App, verb); err != nil {
		return err
	}
	if f.path != "" {
		req.Source = api.JobSource{Path: root}
	} else {
		src, err := upload(ctx, env, c, root, repo, req.App, f.acceptSource)
		if err != nil {
			return err
		}
		req.Source = src
	}

	// Known before the server answers: if the response is lost, the same key returns the same job.
	_, _ = fmt.Fprintf(env.Err, "eve: submitting %s for %s (idempotency key %s; if this command dies, rerun it with --idempotency-key %s)\n",
		verb, req.App, req.IdempotencyKey, req.IdempotencyKey)
	if err := lg.Intend(ledger.Entry{Server: c.BaseURL, Key: req.IdempotencyKey, App: req.App, Verb: verb, Dir: workDir()}); err != nil {
		_, _ = fmt.Fprintln(env.Err, "eve: could not record the submission locally:", err)
	}
	created, err := c.Submit(ctx, req)
	if err != nil {
		return err
	}
	_ = lg.Resolve(c.BaseURL, req.IdempotencyKey, created.ID)
	if !f.wait {
		if env.JSON {
			return env.printJSON(created)
		}
		verbWord := map[api.Verb]string{api.VerbTestflight: "TestFlight", api.VerbDevice: "device", api.VerbCheck: "check", api.VerbExec: "exec"}[verb]
		state := "queued"
		if created.Existing {
			state = "already submitted (" + string(created.State) + ")"
		}
		_, _ = fmt.Fprintf(env.Out, "%s %s %s for %s (idempotency key %s)\n", state, created.ID, verbWord, req.App, created.IdempotencyKey)
		_, _ = fmt.Fprintf(env.Out, "Follow it with: eve wait %s\n", created.ID)
		return nil
	}
	_, _ = fmt.Fprintf(env.Err, "eve: job %s accepted; following it\n", created.ID)
	return follow(ctx, env, c, created.ID, f.follow)
}

// checkVerb refuses, before anything is uploaded, a verb the server cannot run for app.
func checkVerb(ctx context.Context, c *client.Client, app string, verb api.Verb) error {
	apps, err := c.Apps(ctx)
	if err != nil {
		return err
	}
	for _, a := range apps {
		if a.Name != app {
			continue
		}
		if slices.Contains(a.Verbs, verb) {
			return nil
		}
		avail := make([]string, len(a.Verbs))
		for i, v := range a.Verbs {
			avail[i] = string(v)
		}
		return &ExitError{Code: api.ExitUsage, Err: fmt.Errorf("%s is not available for %s on this server (available: %s); nothing was uploaded",
			verb, app, strings.Join(avail, ", "))}
	}
	return &ExitError{Code: api.ExitUsage, Err: fmt.Errorf("app %q is not configured on the server (see eve apps)", app)}
}

// upload lists, size-checks, packs and uploads the tree; it returns the job's source.
func upload(ctx context.Context, env *Env, c *client.Client, root string, repo *config.Repo, app, accept string) (api.JobSource, error) {
	list, err := source.List(root, repo.Ignore)
	if err != nil {
		return api.JobSource{}, err
	}
	if len(list.Files) == 0 {
		return api.JobSource{}, &ExitError{Code: api.ExitUsage, Err: errors.New("nothing to upload")}
	}
	in := sizeguard.SourceInput{Files: sourceFiles(list.Files), Excluded: sourceFiles(list.Excluded)}
	in.Stats = sizeguard.SourceStatsOf(in.Files)
	last, err := c.SourceStats(ctx, app)
	if err != nil {
		return api.JobSource{}, err
	}
	// The server owns the thresholds; the client uses the defaults merged with nothing, which is
	// what every app runs with unless server.yml overrides them (the server re-checks nothing
	// here: the point is to stop before uploading).
	verdict := sizeguard.CheckSource(in, last, config.DefaultSizeGuard().Source)
	if verdict.Tripped && strings.TrimSpace(accept) == "" {
		_, _ = fmt.Fprint(env.Err, verdict.Render())
		_, _ = fmt.Fprintln(env.Err, "Nothing was uploaded.")
		return api.JobSource{}, &ExitError{Code: api.ExitConfirmationRequired}
	}

	tmp, err := os.CreateTemp("", "eve-source-*.tar.zst")
	if err != nil {
		return api.JobSource{}, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer func() { _ = tmp.Close() }()
	info, err := source.Pack(root, list.Files, tmp)
	if err != nil {
		return api.JobSource{}, err
	}
	have, err := c.HasSource(ctx, info.SHA256)
	if err != nil {
		return api.JobSource{}, err
	}
	if !have {
		if _, err := tmp.Seek(0, 0); err != nil {
			return api.JobSource{}, err
		}
		if !env.JSON {
			_, _ = fmt.Fprintf(env.Err, "uploading %s (%d files)\n", sizeguard.FormatBytes(info.Bytes), info.Stats.Files)
		}
		if err := c.PutSource(ctx, info.SHA256, tmp, info.Bytes); err != nil {
			return api.JobSource{}, err
		}
	}
	src := api.JobSource{SHA256: info.SHA256, Stats: info.Stats, AcceptedSize: accept}
	if commit, branch, dirty, ok := source.GitInfo(root); ok {
		src.Commit, src.Branch, src.Dirty = commit, branch, dirty
	}
	return src, nil
}

func sourceFiles(fs []source.File) []sizeguard.SourceFile {
	out := make([]sizeguard.SourceFile, 0, len(fs))
	for _, f := range fs {
		out = append(out, sizeguard.SourceFile{Path: f.Rel, Size: f.Size})
	}
	return out
}

func pick(flag, fromRepo string) string {
	if flag != "" {
		return flag
	}
	return fromRepo
}

func workDir() string {
	d, _ := os.Getwd()
	return d
}
