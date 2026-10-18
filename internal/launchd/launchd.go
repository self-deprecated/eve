// Package launchd installs `eve serve` as a LaunchAgent in the user's GUI (Aqua) session, the
// only place where the login keychain and the Xcode account are usable (PLAN.md §4.1). With Nix,
// the home-manager module (nix/home-manager-module.nix) does this declaratively; `eve agent
// install` is for Macs without it.
package launchd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Label is the LaunchAgent's label.
const Label = "ai.selfdeprecated.eve"

// Spec describes the agent.
type Spec struct {
	Program string // absolute path to the eve binary
	Config  string // server.yml
	LogDir  string
}

// Plist renders the LaunchAgent plist. PATH is Apple's tools only: eve builds its children's
// environment itself and never inherits a shell profile.
func Plist(s Spec) []byte {
	esc := func(v string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		return r.Replace(v)
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + Label + `</string>
  <key>ProgramArguments</key><array>
    <string>` + esc(s.Program) + `</string><string>serve</string><string>--config</string><string>` + esc(s.Config) + `</string>
  </array>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>ProcessType</key><string>Interactive</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>/usr/bin:/bin:/usr/sbin:/sbin</string>
    <key>LANG</key><string>en_US.UTF-8</string>
  </dict>
  <key>StandardOutPath</key><string>` + esc(filepath.Join(s.LogDir, "eve.out.log")) + `</string>
  <key>StandardErrorPath</key><string>` + esc(filepath.Join(s.LogDir, "eve.err.log")) + `</string>
</dict></plist>
`)
}

// Path is where the plist lives.
func Path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// Install writes the plist and (re)loads it into the GUI domain.
func Install(s Spec) error {
	if err := os.MkdirAll(filepath.Dir(Path()), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(s.LogDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(Path(), Plist(s), 0o644); err != nil {
		return err
	}
	_ = launchctl("bootout", domain()+"/"+Label)
	if err := launchctl("bootstrap", domain(), Path()); err != nil {
		return fmt.Errorf("launchctl bootstrap %s (is a GUI session logged in?): %w", domain(), err)
	}
	return nil
}

// Uninstall unloads and removes the agent.
func Uninstall() error {
	_ = launchctl("bootout", domain()+"/"+Label)
	if err := os.Remove(Path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Status returns launchctl's view of the agent.
func Status() (string, error) {
	out, err := exec.Command("/bin/launchctl", "print", domain()+"/"+Label).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("not loaded: %s", strings.TrimSpace(string(out)))
	}
	var keep []string
	for _, l := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(l)
		for _, k := range []string{"state =", "pid =", "last exit code =", "program =", "runs ="} {
			if strings.HasPrefix(t, k) {
				keep = append(keep, t)
			}
		}
	}
	return strings.Join(keep, "\n"), nil
}

// InAquaSession reports whether this process can reach the GUI session's bootstrap namespace.
func InAquaSession() bool {
	out, err := exec.Command("/bin/launchctl", "managername").Output()
	return err == nil && bytes.Equal(bytes.TrimSpace(out), []byte("Aqua"))
}

func launchctl(args ...string) error {
	out, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
