package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/self-deprecated/eve/internal/redact"
	"github.com/self-deprecated/eve/internal/run"
)

// KeySecrets are the secrets `eve apple key create` stores in Doppler; APNS_KEY_P8 is restricted.
var KeySecrets = []string{"APNS_KEY_P8", "APNS_KEY_ID", "APPLE_TEAM_ID"}

// keyEnvironments maps --env to Apple's values; fastlane's documented "development" and
// "production" are rejected.
var keyEnvironments = map[string]string{"sandbox": "sandbox", "production": "prod", "all": "all"}

// envConfigs are the Doppler configs a --doppler target without a config gets, per --env.
var envConfigs = map[string][]string{"sandbox": {"dev"}, "production": {"prd"}, "all": {"dev", "prd"}}

var (
	dopplerNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	secretNameRE  = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// KeySpec is what `eve apple key create` asks for.
type KeySpec struct {
	Name   string
	Env    string   // sandbox, production or all
	Scope  string   // team (every topic) or topic (only Topics)
	Topics []string // bundle ids, for topic keys
	// Doppler targets: "project/config", or "project" for the configs --env implies (sandbox: dev,
	// production: prd, all: dev and prd).
	Doppler []string
}

// Validate checks the spec before anything talks to Apple or Doppler.
func (s KeySpec) Validate() error {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		return usage("--name is required")
	}
	for _, r := range name {
		// Apple rejects punctuation such as parentheses ("Invalid Auth Key name").
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ' ' {
			return usage("--name %q may contain only letters, digits and spaces", s.Name)
		}
	}
	if _, ok := keyEnvironments[s.Env]; !ok {
		return usage("--env must be sandbox, production or all")
	}
	switch s.Scope {
	case "team":
		if len(s.Topics) > 0 {
			return usage("--topic is only for --scope topic")
		}
	case "topic":
		if len(s.Topics) == 0 {
			return usage("--scope topic needs at least one --topic <bundle-id>")
		}
		if s.Env == "all" {
			return usage("Apple allows topic keys in one environment only: use --env sandbox or --env production")
		}
		for _, t := range s.Topics {
			if !bundleIDRE.MatchString(t) {
				return usage("--topic %q must be a bundle id", t)
			}
		}
	default:
		return usage("--scope must be team or topic")
	}
	_, err := s.targets()
	return err
}

// named is a Doppler project or config in `--json` listings.
type named struct {
	Name string `json:"name"`
}

// target is one Doppler config.
type target struct{ Project, Config string }

func (t target) String() string { return t.Project + "/" + t.Config }

// targets expands --doppler into project/config pairs, in order and without duplicates.
func (s KeySpec) targets() ([]target, error) {
	if len(s.Doppler) == 0 {
		return nil, usage("--doppler <project>[/<config>] is required: where the key is stored")
	}
	var out []target
	for _, d := range s.Doppler {
		project, config, hasConfig := strings.Cut(d, "/")
		configs := envConfigs[s.Env]
		if hasConfig {
			configs = []string{config}
		}
		if !dopplerNameRE.MatchString(project) {
			return nil, usage("--doppler %q must be <project> or <project>/<config>", d)
		}
		for _, c := range configs {
			if !dopplerNameRE.MatchString(c) {
				return nil, usage("--doppler %q must be <project> or <project>/<config>", d)
			}
			if t := (target{project, c}); !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

// KeyResult is what `eve apple key create` did.
type KeyResult struct {
	Account
	KeyID  string   `json:"keyId,omitempty"`
	Name   string   `json:"name"`
	Env    string   `json:"env"`
	Scope  string   `json:"scope"`
	Topics []string `json:"topics,omitempty"`
	// Stored lists the Doppler configs that have all three secrets.
	Stored []string `json:"stored"`
	// CreatedProjects are Doppler projects that didn't exist.
	CreatedProjects []string `json:"createdProjects,omitempty"`
	// RescuePath is the only copy of the key when storing it failed.
	RescuePath string `json:"rescuePath,omitempty"`
}

// CreateKey creates an APNs key and stores APNS_KEY_P8 (restricted), APNS_KEY_ID and APPLE_TEAM_ID
// in each Doppler config with the Doppler CLI's own login. Doppler is checked (and missing
// projects created) first, so a key is only created when it can be stored. Apple serves the .p8
// exactly once: it waits in a 0600 file under RescueDir until every config has it, and stays there
// (named in the error) if storing fails.
func (c *Client) CreateKey(ctx context.Context, team string, s KeySpec) (KeyResult, error) {
	res := KeyResult{Name: strings.TrimSpace(s.Name), Env: s.Env, Scope: s.Scope, Topics: s.Topics, Stored: []string{}}
	if err := s.Validate(); err != nil {
		return res, err
	}
	targets, _ := s.targets()
	req, err := c.request(team, map[string]any{
		"name": res.Name, "environment": keyEnvironments[s.Env], "scope": s.Scope, "topics": nonNil(s.Topics),
		"rescueDir": c.RescueDir,
	})
	if err != nil {
		return res, err
	}
	doppler, created, err := c.prepareDoppler(ctx, targets)
	res.CreatedProjects = created
	if err != nil {
		return res, err
	}
	var key struct {
		Account
		KeyID      string `json:"keyId"`
		RescuePath string `json:"rescuePath"`
	}
	err = c.lane(ctx, "apns_key", req, &key)
	res.Account, res.KeyID = key.Account, key.KeyID
	if err != nil {
		return res, err
	}
	if filepath.Dir(key.RescuePath) != filepath.Clean(c.RescueDir) {
		return res, &Error{Kind: KindFailed, Message: fmt.Sprintf("created APNs key %s but fastlane saved it to an unexpected path %q", key.KeyID, key.RescuePath)}
	}
	res.RescuePath = key.RescuePath
	p8, err := os.ReadFile(key.RescuePath)
	if err != nil {
		return res, &Error{Kind: KindFailed, Message: fmt.Sprintf("created APNs key %s but can't read %s: %v", key.KeyID, key.RescuePath, err)}
	}
	values := map[string][]byte{"APNS_KEY_P8": p8, "APNS_KEY_ID": []byte(key.KeyID), "APPLE_TEAM_ID": []byte(key.Team)}
	for _, t := range targets {
		for _, name := range KeySecrets {
			if err := c.dopplerSet(ctx, doppler, t, name, values[name], p8); err != nil {
				return res, &Error{Kind: KindFailed, Message: fmt.Sprintf(
					"created APNs key %s, but Doppler didn't store %s in %s: %v\nthe key is in %s (0600), its only copy: store it, then delete the file",
					key.KeyID, name, t, err, key.RescuePath)}
			}
		}
		res.Stored = append(res.Stored, t.String())
	}
	if err := os.Remove(key.RescuePath); err != nil {
		return res, &Error{Kind: KindFailed, Message: fmt.Sprintf("stored APNs key %s but couldn't delete %s: %v", key.KeyID, key.RescuePath, err)}
	}
	res.RescuePath = ""
	return res, nil
}

// prepareDoppler finds the Doppler CLI, creates missing projects and checks every config exists.
// It reads only project and config names.
func (c *Client) prepareDoppler(ctx context.Context, targets []target) (string, []string, error) {
	path, err := c.LookPath("doppler")
	if err != nil {
		return "", nil, &Error{Kind: KindFailed, Message: "doppler is not on PATH; install the Doppler CLI and run `doppler login`"}
	}
	var projects []named
	if err := c.dopplerJSON(ctx, path, &projects, "projects", "--json"); err != nil {
		return path, nil, err
	}
	var created []string
	for _, t := range targets {
		known := slices.ContainsFunc(projects, func(p named) bool { return p.Name == t.Project })
		if known || slices.Contains(created, t.Project) {
			continue
		}
		_, errOut, code, err := c.capture(ctx, path, "projects", "create", t.Project, "--silent",
			"--description", "Created by eve apple key create")
		if err != nil {
			return path, created, err
		}
		if code != 0 {
			return path, created, &Error{Kind: KindFailed, Message: fmt.Sprintf("doppler couldn't create project %s: %s", t.Project, tail(strings.TrimSpace(errOut), 4))}
		}
		created = append(created, t.Project)
	}
	configs := map[string][]string{}
	for _, t := range targets {
		if _, ok := configs[t.Project]; !ok {
			var list []named
			if err := c.dopplerJSON(ctx, path, &list, "configs", "--project", t.Project, "--json"); err != nil {
				return path, created, err
			}
			configs[t.Project] = []string{}
			for _, l := range list {
				configs[t.Project] = append(configs[t.Project], l.Name)
			}
		}
		if !slices.Contains(configs[t.Project], t.Config) {
			return path, created, usage("Doppler project %s has no config %s (it has %s)", t.Project, t.Config, strings.Join(configs[t.Project], ", "))
		}
	}
	return path, created, nil
}

func (c *Client) dopplerJSON(ctx context.Context, path string, out any, args ...string) error {
	stdout, errOut, code, err := c.capture(ctx, path, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return &Error{Kind: KindFailed, Message: fmt.Sprintf("doppler %s failed (signed in? `doppler login`): %s", args[0], tail(strings.TrimSpace(errOut+stdout), 4))}
	}
	if err := json.Unmarshal([]byte(stdout), out); err != nil {
		return fmt.Errorf("doppler %s: %w", args[0], err)
	}
	return nil
}

// dopplerSet stores one value, passed on stdin and never in argv. Doppler's stdout can echo values,
// so it is discarded; its stderr is only shown on failure, redacted.
func (c *Client) dopplerSet(ctx context.Context, path string, t target, name string, value, p8 []byte) error {
	args := []string{"secrets", "set", name, "--project", t.Project, "--config", t.Config, "--no-interactive", "--silent"}
	if name == "APNS_KEY_P8" {
		args = append(args, "--visibility", "restricted")
	}
	var errOut bytes.Buffer
	res, err := c.Runner.Run(ctx, run.Cmd{
		Name: path, Args: args, Env: slices.Clone(c.Environ),
		Stdin: bytes.NewReader(value), Stdout: io.Discard, Stderr: &errOut,
	})
	if err != nil {
		return err
	}
	if res.ExitCode == 0 {
		return nil
	}
	secrets := []string{string(p8), strings.TrimSpace(string(p8))}
	for _, line := range strings.Split(string(p8), "\n") {
		secrets = append(secrets, strings.TrimSpace(line))
	}
	msg := strings.TrimSpace(redact.New(redact.Options{Secrets: secrets}).String(errOut.String()))
	return fmt.Errorf("doppler exited %d: %s", res.ExitCode, tail(msg, 6))
}

func (c *Client) capture(ctx context.Context, name string, args ...string) (string, string, int, error) {
	return c.captureEnv(ctx, slices.Clone(c.Environ), name, args...)
}

func (c *Client) captureEnv(ctx context.Context, env []string, name string, args ...string) (string, string, int, error) {
	var out, errOut bytes.Buffer
	res, err := c.Runner.Run(ctx, run.Cmd{Name: name, Args: args, Env: env, Stdout: &out, Stderr: &errOut})
	return out.String(), errOut.String(), res.ExitCode, err
}
