package run

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Fake is a Runner for tests. Each call is recorded; Handler decides the outcome.
type Fake struct {
	mu      sync.Mutex
	Calls   []Cmd
	Handler func(c Cmd) (stdout string, exitCode int, err error)
}

// Run records c and returns what Handler says (exit 0 with no output when Handler is nil).
func (f *Fake) Run(ctx context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Result{ExitCode: -1}, err
	}
	if c.OnStart != nil {
		c.OnStart(4242)
	}
	if f.Handler == nil {
		return Result{Pgid: 4242}, nil
	}
	out, code, err := f.Handler(c)
	if out != "" && c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, out)
	}
	return Result{ExitCode: code, Pgid: 4242}, err
}

// Commands returns the recorded command lines.
func (f *Fake) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = c.String()
	}
	return out
}

// Find returns the first recorded command whose line contains substr.
func (f *Fake) Find(substr string) (Cmd, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if strings.Contains(c.String(), substr) {
			return c, nil
		}
	}
	return Cmd{}, fmt.Errorf("no command containing %q", substr)
}
