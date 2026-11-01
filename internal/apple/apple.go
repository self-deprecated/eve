// Package apple provisions an app on Apple's side from the machine running the eve CLI: bundle
// ids, capabilities, app groups, App Store Connect records and APNs keys (docs/design.md §20).
//
// It runs fastlane with an embedded Fastfile over the Apple ID web session Spaceship caches under
// eve's config directory; App Store Connect API keys can create neither app records nor APNs keys.
// The eve server is not involved. Only `eve apple login` asks for a password, in the terminal;
// every other command reuses the cached session and ends needs_human (exit 78) when it expired.
package apple

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/run"
)

//go:embed Fastfile
var fastfile []byte

// Error kinds a lane reports, and the exit codes they map to (docs/design.md §5.2).
const (
	KindSession     = "session"     // no valid Apple session: run `eve apple login`
	KindAgreement   = "agreement"   // Apple wants an agreement accepted
	KindUsage       = "usage"       // bad arguments, or something the request names doesn't exist
	KindRejected    = "rejected"    // Apple refused the change
	KindUnavailable = "unavailable" // Apple or the network unreachable
	KindFailed      = "failed"      // anything else, including a missing tool
)

// Error is a failed Apple command.
type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Action  string `json:"action,omitempty"`
	URL     string `json:"url,omitempty"`
	// Log is the end of fastlane's output when it stopped without writing a result.
	Log string `json:"-"`
}

func (e *Error) Error() string {
	msg := e.Message
	if e.Kind == KindSession {
		msg += "; run `eve apple login`"
	}
	if e.Log != "" {
		msg += "\n" + e.Log
	}
	return msg
}

// ExitCode maps the error to eve's exit codes.
func (e *Error) ExitCode() int {
	switch e.Kind {
	case KindSession, KindAgreement:
		return api.ExitNeedsHuman
	case KindUsage:
		return api.ExitUsage
	case KindRejected:
		return api.ExitRejected
	case KindUnavailable:
		return api.ExitUnavailable
	}
	return api.ExitFailed
}

// Outcome is the job-style outcome name for --json output.
func (e *Error) Outcome() api.State {
	switch e.ExitCode() {
	case api.ExitNeedsHuman:
		return api.StateNeedsHuman
	case api.ExitRejected:
		return api.StateRejected
	case api.ExitUnavailable:
		return api.StateUnavailable
	}
	return api.StateFailed
}

// Human is what the user has to do, for needs_human errors.
func (e *Error) Human() *api.Human {
	switch e.Kind {
	case KindSession:
		return &api.Human{Action: "apple_login", Detail: "run `eve apple login`"}
	case KindAgreement:
		return &api.Human{Action: e.Action, Detail: e.Message, URL: e.URL}
	}
	return nil
}

func usage(format string, args ...any) *Error {
	return &Error{Kind: KindUsage, Message: fmt.Sprintf(format, args...)}
}

// Client runs the lanes. The zero value is not usable; see New.
type Client struct {
	// Runner runs fastlane for every lane but login, and secretspec.
	Runner run.Runner
	// Interactive runs `fastlane login` attached to the terminal (password and 2FA prompts).
	Interactive run.Runner
	LookPath    func(string) (string, error)
	// Environ is the environment commands inherit (os.Environ()).
	Environ []string
	// Dir holds the Apple ID and Spaceship's cookie (<config>/apple, 0700).
	Dir string
	// RescueDir is where a new key's .p8 waits until it is stored.
	RescueDir string
	// Version is eve's, passed to secretspec as --caller-version.
	Version string
}

// New returns a Client for this machine; dir is eve's client config directory.
func New(configDir, version string) *Client {
	rescue := os.Getenv("XDG_RUNTIME_DIR")
	if rescue == "" {
		rescue = os.TempDir()
	}
	return &Client{
		Runner: run.Exec{}, Interactive: terminal{}, LookPath: exec.LookPath, Environ: os.Environ(),
		Dir: filepath.Join(configDir, "apple"), RescueDir: rescue, Version: version,
	}
}

// terminal runs a command in the foreground with the caller's terminal: run.Exec puts commands in
// their own process group, which can't read a password from the terminal.
type terminal struct{}

func (terminal) Run(ctx context.Context, c run.Cmd) (run.Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir, cmd.Env = c.Dir, c.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return run.Result{ExitCode: exitErr.ExitCode()}, nil
	}
	return run.Result{}, err
}

// Team is one of the Apple account's teams.
type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Account is the team a lane worked in.
type Account struct {
	Team     string `json:"team,omitempty"`
	TeamName string `json:"teamName,omitempty"`
}

func (c *Client) appleIDPath() string { return filepath.Join(c.Dir, "apple-id") }

// AppleID is the Apple ID `eve apple login` signed in with, or "".
func (c *Client) AppleID() string {
	b, err := os.ReadFile(c.appleIDPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (c *Client) ensureDir() error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(c.Dir, 0o700)
}

// request is what every lane gets; lane-specific fields are merged in.
func (c *Client) request(team string, fields map[string]any) (map[string]any, error) {
	id := c.AppleID()
	if id == "" {
		return nil, &Error{Kind: KindSession, Message: "not signed in to Apple on this machine"}
	}
	req := map[string]any{"appleId": id, "team": team}
	for k, v := range fields {
		req[k] = v
	}
	return req, nil
}

// lane runs one lane non-interactively and decodes its result into out. A lane that fails after
// doing something still fills out with what it did.
func (c *Client) lane(ctx context.Context, name string, req map[string]any, out any) error {
	return c.runLane(ctx, false, name, req, out)
}

func (c *Client) runLane(ctx context.Context, interactive bool, name string, req map[string]any, out any) error {
	fastlane, err := c.LookPath("fastlane")
	if err != nil {
		return &Error{Kind: KindFailed, Message: "fastlane is not on PATH; install it (brew install fastlane, or nix profile install nixpkgs#fastlane)"}
	}
	if err := c.ensureDir(); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "eve-apple-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	if err := os.Mkdir(filepath.Join(work, "fastlane"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(work, "fastlane", "Fastfile"), fastfile, 0o600); err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	reqPath, resPath := filepath.Join(work, "request.json"), filepath.Join(work, "result.json")
	if err := os.WriteFile(reqPath, body, 0o600); err != nil {
		return err
	}
	var log bytes.Buffer
	cmd := run.Cmd{Name: fastlane, Args: []string{name}, Dir: work, Env: c.env(reqPath, resPath)}
	r := c.Interactive
	if !interactive {
		r = c.Runner
		cmd.Stdout, cmd.Stderr = &log, &log
	}
	res, err := r.Run(ctx, cmd)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Kind: KindFailed, Message: fmt.Sprintf("fastlane %s: %v", name, err)}
	}
	raw, err := os.ReadFile(resPath)
	if err != nil {
		return &Error{Kind: KindFailed, Message: fmt.Sprintf("fastlane %s exited %d without a result", name, res.ExitCode), Log: tail(log.String(), 20)}
	}
	var doc struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("fastlane %s: unreadable result: %w", name, err)
	}
	if len(doc.Result) > 0 && out != nil {
		if err := json.Unmarshal(doc.Result, out); err != nil {
			return fmt.Errorf("fastlane %s: unreadable result: %w", name, err)
		}
	}
	if doc.Error != nil {
		return doc.Error
	}
	return nil
}

// env is the caller's environment with fastlane quiet, offline from its update and analytics
// services, and Spaceship's cookie under eve's directory instead of ~/.fastlane.
func (c *Client) env(reqPath, resPath string) []string {
	set := map[string]string{
		"SPACESHIP_COOKIE_PATH":      c.Dir,
		"FASTLANE_SKIP_UPDATE_CHECK": "1",
		"FASTLANE_OPT_OUT_USAGE":     "1",
		"FASTLANE_SKIP_DOCS":         "1",
		"FASTLANE_HIDE_TIMESTAMP":    "1",
		"FASTLANE_DISABLE_COLORS":    "1",
		"EVE_APPLE_REQUEST":          reqPath,
		"EVE_APPLE_RESULT":           resPath,
	}
	env := make([]string, 0, len(c.Environ)+len(set)+2)
	hasLang := false
	for _, kv := range c.Environ {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := set[k]; ok {
			continue
		}
		hasLang = hasLang || k == "LANG" || k == "LC_ALL"
		env = append(env, kv)
	}
	for k, v := range set {
		env = append(env, k+"="+v)
	}
	if !hasLang {
		env = append(env, "LANG=en_US.UTF-8") // fastlane warns and misbehaves without a UTF-8 locale
	}
	return env
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// LoginResult is what `eve apple login` reports.
type LoginResult struct {
	Account
	AppleID  string `json:"appleId"`
	SignedIn bool   `json:"signedIn"` // false: the cached session was still valid
	Teams    []Team `json:"teams,omitempty"`
}

// Login signs in interactively (password and 2FA in the terminal) unless the cached session for
// appleID is still valid, then remembers appleID for the other commands.
func (c *Client) Login(ctx context.Context, appleID, team string) (LoginResult, error) {
	res := LoginResult{AppleID: appleID}
	err := c.runLane(ctx, true, "login", map[string]any{"appleId": appleID, "team": team}, &res)
	if err != nil {
		return res, err
	}
	res.AppleID = appleID
	if err := os.WriteFile(c.appleIDPath(), []byte(appleID+"\n"), 0o600); err != nil {
		return res, err
	}
	// Spaceship writes the cookie with the umask the Fastfile sets; make sure of it.
	cookieDir := filepath.Join(c.Dir, "spaceship")
	_ = filepath.WalkDir(cookieDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return os.Chmod(p, 0o700)
		}
		return os.Chmod(p, 0o600)
	})
	return res, nil
}

// App is an App Store Connect app record.
type App struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BundleID string `json:"bundleId"`
	SKU      string `json:"sku"`
	Locale   string `json:"locale"`
}

// AppList is `eve apple apps`.
type AppList struct {
	Account
	Apps []App `json:"apps"`
}

// Apps lists the team's App Store Connect apps.
func (c *Client) Apps(ctx context.Context, team string) (AppList, error) {
	var out AppList
	req, err := c.request(team, nil)
	if err != nil {
		return out, err
	}
	err = c.lane(ctx, "apps", req, &out)
	return out, err
}

// Key is a key in the developer account (never its material).
type Key struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Services []string `json:"services"`
}

// KeyList is `eve apple keys`.
type KeyList struct {
	Account
	Keys []Key `json:"keys"`
}

// Keys lists the team's keys.
func (c *Client) Keys(ctx context.Context, team string) (KeyList, error) {
	var out KeyList
	req, err := c.request(team, nil)
	if err != nil {
		return out, err
	}
	err = c.lane(ctx, "keys", req, &out)
	return out, err
}
