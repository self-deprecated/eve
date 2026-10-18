// Package cli implements the eve command line (docs/design.md §10). Output is human-readable by default
// and JSON with --json; exit codes follow docs/design.md §5.2.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/client"
	"github.com/spf13/cobra"
)

// ExitError carries a specific exit code out of a command.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// Env is shared state for commands.
type Env struct {
	Version string
	JSON    bool
	Out     io.Writer
	Err     io.Writer
	// NewClient builds the API client (replaced in tests).
	NewClient func() (*client.Client, error)
}

// Root builds the command tree. Server-side commands are added by AddServerCommands.
func Root(version string) (*cobra.Command, *Env) {
	env := &Env{Version: version, Out: os.Stdout, Err: os.Stderr, NewClient: client.FromConfig}
	root := &cobra.Command{
		Use:           "eve",
		Short:         "Fire-and-forget iOS builds on your own Mac: TestFlight, device installs and checks",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&env.JSON, "json", false, "print machine-readable JSON")
	root.AddCommand(
		versionCmd(env), loginCmd(env), doctorCmd(env), waitCmd(env), statusCmd(env), logsCmd(env),
		jobsCmd(env), cancelCmd(env), confirmCmd(env), rejectCmd(env), sizeCmd(env), buildsCmd(env),
		baselineCmd(env), appsCmd(env), submitCmd(env), pendingCmd(env), initCmd(env), artifactsCmd(env),
	)
	return root, env
}

// Main runs the CLI and returns the process exit code.
func Main(root *cobra.Command, env *Env) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		if ee.Err != nil {
			_, _ = fmt.Fprintln(env.Err, "eve:", ee.Err)
		}
		return ee.Code
	}
	_, _ = fmt.Fprintln(env.Err, "eve:", err)
	var apiErr *client.APIError
	var unreachable *client.ErrUnreachable
	switch {
	case errors.As(err, &unreachable):
		return api.ExitUnavailable
	case errors.As(err, &apiErr) && apiErr.Status >= 500:
		return api.ExitUnavailable
	case errors.As(err, &apiErr) && apiErr.Status == 400:
		return api.ExitUsage
	}
	return api.ExitFailed
}

func (e *Env) client() (*client.Client, error) { return e.NewClient() }

func (e *Env) printJSON(v any) error {
	enc := json.NewEncoder(e.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeResult prints a result document (JSON or human).
func (e *Env) writeResult(r api.Result) error {
	if e.JSON {
		return e.printJSON(r)
	}
	_, err := fmt.Fprint(e.Out, FormatResult(r))
	return err
}

// printResult prints a result document and returns the exit error matching its outcome.
func (e *Env) printResult(r api.Result) error {
	if err := e.writeResult(r); err != nil {
		return err
	}
	if code := api.ExitCode(r.Outcome); code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
