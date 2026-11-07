package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/apple"
	"github.com/self-deprecated/eve/internal/run"
)

func runApple(t *testing.T, handler func(run.Cmd) (string, int, error), signedIn bool, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	f := &run.Fake{Handler: handler}
	a := &apple.Client{
		Runner: f, Interactive: f, Dir: filepath.Join(dir, "apple"), RescueDir: dir,
		LookPath: func(name string) (string, error) { return "/bin/" + name, nil },
	}
	if signedIn {
		_ = os.MkdirAll(a.Dir, 0o700)
		_ = os.WriteFile(filepath.Join(a.Dir, "apple-id"), []byte("sam@example.com\n"), 0o600)
	}
	var out bytes.Buffer
	root, env := Root("test")
	env.Out, env.Err = &out, &out
	env.NewApple = func() *apple.Client { return a }
	root.SetArgs(append([]string{"apple"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func exitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

func TestAppleCommandsExitCodes(t *testing.T) {
	_, err := runApple(t, nil, true, "key", "create", "--name", "Push (prod)", "--env", "all", "--doppler", "intentpal")
	if exitCode(err) != api.ExitUsage {
		t.Fatal("bad key name:", err)
	}
	out, err := runApple(t, nil, false, "apps", "--json")
	if exitCode(err) != api.ExitNeedsHuman {
		t.Fatal("not signed in:", err)
	}
	var doc struct {
		Outcome api.State
		Human   api.Human
	}
	if json.Unmarshal([]byte(out), &doc) != nil || doc.Outcome != api.StateNeedsHuman || doc.Human.Detail != "run `eve apple login`" {
		t.Fatal(out)
	}
}

func TestAppleAppCreateReportsNoChanges(t *testing.T) {
	result := `{"result":{"team":"TEAM123456","bundleId":"com.example.intentpal","appId":"6829483604","changes":[]}}`
	out, err := runApple(t, func(c run.Cmd) (string, int, error) {
		for _, kv := range c.Env {
			if p, ok := strings.CutPrefix(kv, "EVE_APPLE_RESULT="); ok {
				return "", 0, os.WriteFile(p, []byte(result), 0o600)
			}
		}
		return "", 1, nil
	}, true, "app", "create", "com.example.intentpal", "--name", "Intent Pal", "--sku", "intentpal", "--like", "com.example.screengate")
	if err != nil {
		t.Fatal(err)
	}
	if out != "no changes: com.example.intentpal already has everything on Apple's side (team TEAM123456, app record 6829483604)\n" {
		t.Fatalf("%q", out)
	}
}

// Tests run without a terminal on stdin, as an agent's shell does.
func TestAppleLoginWithoutATerminalNeedsThePasswordFromDoppler(t *testing.T) {
	t.Setenv("EVE_APPLE_PASSWORD_DOPPLER", "")
	_, err := runApple(t, nil, true, "login")
	if exitCode(err) != api.ExitUsage || !strings.Contains(err.Error(), "--password-doppler") {
		t.Fatal("no terminal, no Doppler:", err)
	}
	_, err = runApple(t, nil, false, "login", "--password-doppler", "apple/prd/APPLE_ID_PASSWORD")
	if exitCode(err) != api.ExitUsage || !strings.Contains(err.Error(), "--apple-id") {
		t.Fatal("no terminal to ask for the Apple ID:", err)
	}
	handler := func(result string) func(run.Cmd) (string, int, error) {
		return func(c run.Cmd) (string, int, error) {
			if strings.HasSuffix(c.Name, "doppler") {
				if len(c.Args) > 2 && c.Args[2] == "create" {
					return `{"token":"dp.st.prd.x","slug":"s"}`, 0, nil
				}
				return "hunter2\n", 0, nil
			}
			for _, kv := range c.Env {
				if p, ok := strings.CutPrefix(kv, "EVE_APPLE_RESULT="); ok {
					return "", 0, os.WriteFile(p, []byte(result), 0o600)
				}
			}
			return "", 1, nil
		}
	}
	out, err := runApple(t, handler(`{"result":{"signedIn":true,"team":"TEAM123456"}}`), true, "login", "--password-doppler", "apple/prd/APPLE_ID_PASSWORD")
	if err != nil || !strings.Contains(out, "signed in to Apple as sam@example.com") {
		t.Fatal(out, err)
	}
	// Apple asked for a two-factor code the lane can't prompt for.
	out, err = runApple(t, handler(`{"error":{"kind":"session","message":"Apple asked for a two-factor code, which needs a terminal"}}`), true, "login", "--json", "--password-doppler", "apple/prd/APPLE_ID_PASSWORD")
	var doc struct {
		Outcome api.State
		Human   api.Human
	}
	if exitCode(err) != api.ExitNeedsHuman || json.Unmarshal([]byte(out), &doc) != nil || doc.Outcome != api.StateNeedsHuman || doc.Human.Action != "apple_login" {
		t.Fatal(out, err)
	}
}
