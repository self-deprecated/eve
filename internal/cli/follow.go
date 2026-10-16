package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/client"
	"github.com/self-deprecated/eve/internal/ledger"
	"github.com/spf13/cobra"
)

// waitFlags are shared by `eve wait` and `eve submit --wait`.
type waitFlags struct {
	timeout time.Duration
	quiet   bool
	events  bool
}

func (w *waitFlags) register(cmd *cobra.Command) {
	cmd.Flags().DurationVar(&w.timeout, "timeout", 0, "stop waiting after this long (exit 75); the job keeps running")
	cmd.Flags().BoolVarP(&w.quiet, "quiet", "q", false, "no progress lines")
	cmd.Flags().BoolVar(&w.events, "events", false,
		"milestone mode: one stdout line per phase change and a final one-line result (for Claude Code's Monitor)")
}

// follow waits for a job and prints the outcome; the returned error carries the exit code.
//
// Default mode: progress lines on stderr, the result document on stdout (human, or --json).
// Events mode: every line is on stdout and stands alone, so a harness can wake an agent per
// line: "eve <job> <state>: <message>" for each phase change, then
// "eve <job> result <OUTCOME> <compact JSON>".
func follow(ctx context.Context, env *Env, c *client.Client, id string, w waitFlags) error {
	if w.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.timeout)
		defer cancel()
	}
	opts := client.WaitOptions{
		OnEvent: func(ev api.Event) {
			switch {
			case w.events:
				if ev.Kind == api.EventTransition && ev.State != api.StateQueued {
					_, _ = fmt.Fprintln(env.Out, eventLine(id, ev))
				}
			case !w.quiet && !env.JSON && ev.Message != "":
				_, _ = fmt.Fprintf(env.Err, "%s  %-22s %s\n", ev.Time.Local().Format("15:04:05"), ev.State, ev.Message)
			}
		},
		OnReconnect: func(err error, delay time.Duration) {
			if !w.quiet {
				_, _ = fmt.Fprintf(env.Err, "eve: connection lost (%v); reconnecting in %s\n", shortErr(err), delay)
			}
		},
	}
	r, err := c.Wait(ctx, id, opts)
	if errors.Is(err, context.DeadlineExceeded) {
		if w.events {
			_, _ = fmt.Fprintf(env.Out, "eve %s still %s after %s; the job keeps running (eve wait %s)\n", id, r.Outcome, w.timeout, id)
		}
		return &ExitError{Code: api.ExitWaitTimeout, Err: fmt.Errorf("still %s after %s; the job keeps running (eve wait %s)", r.Outcome, w.timeout, id)}
	}
	if err != nil {
		return err
	}
	var perr error
	if w.events {
		_, perr = fmt.Fprintf(env.Out, "eve %s result %s %s\n", id, strings.ToUpper(string(r.Outcome)), compactResult(r))
	} else {
		perr = env.writeResult(r)
	}
	if perr != nil {
		return perr // not shown, so not collected
	}
	_ = ledger.Default().Collect(c.BaseURL, r.ID, r.Outcome)
	if code := api.ExitCode(r.Outcome); code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// maxResultLine bounds the final events-mode line for per-line watchers.
const maxResultLine = 6000

// compactResult is the result as one JSON line, trimmed of bulky optional parts if needed.
func compactResult(r api.Result) []byte {
	b, _ := json.Marshal(r)
	if len(b) <= maxResultLine {
		return b
	}
	if r.Size != nil {
		sz := *r.Size
		sz.Largest = nil
		r.Size = &sz
	}
	if r.Failure != nil {
		f := *r.Failure
		if len(f.Excerpt) > 10 {
			f.Excerpt = f.Excerpt[:10]
		}
		r.Failure = &f
	}
	r.Artifacts = nil
	b, _ = json.Marshal(r)
	if len(b) <= maxResultLine {
		return b
	}
	b, _ = json.Marshal(api.Result{
		ID: r.ID, App: r.App, Verb: r.Verb, Outcome: r.Outcome, Phase: r.Phase, Build: r.Build, Human: r.Human,
		Failure: &api.Failure{Class: failureClass(r), Summary: "result too large for one line; run eve status " + r.ID},
	})
	return b
}

func failureClass(r api.Result) string {
	if r.Failure != nil {
		return r.Failure.Class
	}
	return ""
}

// eventLine renders one phase change for events mode.
func eventLine(id string, ev api.Event) string {
	msg := strings.Join(strings.Fields(ev.Message), " ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return fmt.Sprintf("eve %s %s: %s", id, ev.State, msg)
}

func shortErr(err error) string {
	if err == nil {
		return "stream ended"
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func pendingCmd(env *Env) *cobra.Command {
	var peek bool
	cmd := &cobra.Command{
		Use:   "pending",
		Short: "Jobs submitted from this machine whose outcome nobody has collected yet",
		Long: "Lists submissions from this machine whose outcome nobody has seen (for example because\n" +
			"the waiting process died). Finished ones are printed and then marked collected, unless --peek.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lg := ledger.Default()
			entries, err := lg.Pending()
			if err != nil {
				return err
			}
			c, cerr := env.client()
			type item struct {
				Entry  ledger.Entry `json:"entry"`
				Result *api.Result  `json:"result,omitempty"`
				Error  string       `json:"error,omitempty"`
			}
			var items []item
			for _, e := range entries {
				it := item{Entry: e}
				switch {
				case e.ID == "":
					it.Error = fmt.Sprintf("the server never confirmed this submission; rerun the same submit with --idempotency-key %s", e.Key)
				case cerr != nil:
					it.Error = cerr.Error()
				case e.Server != c.BaseURL:
					it.Error = "submitted to " + e.Server + ", not the current server; log in there to check it"
				default:
					r, err := c.Job(cmd.Context(), e.ID)
					var apiErr *client.APIError
					switch {
					case errors.As(err, &apiErr) && apiErr.Status == 404:
						it.Error = "unknown to " + e.Server + " (pruned?)"
						if !peek {
							_ = lg.Drop(e.Server, e.ID, it.Error)
						}
					case err != nil:
						it.Error = err.Error()
					default:
						it.Result = &r
					}
				}
				items = append(items, it)
			}
			collect := func(it item) {
				if !peek && it.Result != nil {
					_ = lg.Collect(it.Entry.Server, it.Result.ID, it.Result.Outcome)
				}
			}
			if env.JSON {
				if items == nil {
					items = []item{}
				}
				if err := env.printJSON(items); err != nil {
					return err
				}
				for _, it := range items {
					collect(it)
				}
				return nil
			}
			if len(items) == 0 {
				_, _ = fmt.Fprintln(env.Out, "nothing pending")
				return nil
			}
			for _, it := range items {
				var line string
				if it.Result != nil {
					line = pendingLine(*it.Result)
				} else {
					line = fmt.Sprintf("%s  %s %s  ? %s\n", orKey(it.Entry), it.Entry.Verb, it.Entry.App, it.Error)
				}
				if _, err := fmt.Fprint(env.Out, line); err != nil {
					return err
				}
				collect(it)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&peek, "peek", false, "don't mark finished jobs as collected")
	return cmd
}

func orKey(e ledger.Entry) string {
	if e.ID != "" {
		return e.ID
	}
	return "(key " + e.Key + ")"
}

func pendingLine(r api.Result) string {
	switch {
	case r.Outcome == api.StateAwaitingConfirmation:
		return fmt.Sprintf("%s  %s %s  NEEDS CONFIRMATION  eve size %s, then eve confirm/reject\n", r.ID, r.Verb, r.App, r.ID)
	case r.Outcome.Terminal():
		return FormatResult(r)
	}
	return fmt.Sprintf("%s  %s %s  still %s  (eve wait %s)\n", r.ID, r.Verb, r.App, r.Outcome, r.ID)
}
