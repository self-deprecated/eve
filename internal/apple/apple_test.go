package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/run"
)

const p8 = "-----BEGIN PRIVATE KEY-----\nMIGTAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBHkwdwIBAQQgSECRETSECRETSECRET\nqA0Kc2VjcmV0LWtleS1tYXRlcmlhbC1mb3ItdGVzdHM=\n-----END PRIVATE KEY-----\n"

func newClient(t *testing.T, handler func(run.Cmd) (string, int, error)) (*Client, *run.Fake) {
	t.Helper()
	f := &run.Fake{Handler: handler}
	c := &Client{
		Runner: f, Interactive: f, Environ: []string{"PATH=/usr/bin", "HOME=/home/sam", "FASTLANE_OPT_OUT_USAGE=0"},
		LookPath: func(name string) (string, error) { return "/bin/" + name, nil },
		Dir:      filepath.Join(t.TempDir(), "apple"), RescueDir: t.TempDir(), Version: "1.2.3",
	}
	return c, f
}

func signIn(t *testing.T, c *Client) {
	t.Helper()
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "apple-id"), []byte("sam@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envValue(c run.Cmd, key string) (string, int) {
	var v string
	n := 0
	for _, kv := range c.Env {
		if val, ok := strings.CutPrefix(kv, key+"="); ok {
			v, n = val, n+1
		}
	}
	return v, n
}

// lane answers fastlane with result, and records the request it got.
func lane(result string, req *map[string]any) func(run.Cmd) (string, int, error) {
	return func(c run.Cmd) (string, int, error) {
		if !strings.HasSuffix(c.Name, "fastlane") {
			return "", 0, nil
		}
		if req != nil {
			path, _ := envValue(c, "EVE_APPLE_REQUEST")
			b, _ := os.ReadFile(path)
			_ = json.Unmarshal(b, req)
		}
		path, _ := envValue(c, "EVE_APPLE_RESULT")
		return "[17:00:00]: Driving the lane\n", 0, os.WriteFile(path, []byte(result), 0o600)
	}
}

func TestLaneRunsTheEmbeddedFastfileWithEvesCookieDir(t *testing.T) {
	var fastfileSeen []byte
	var workDir string
	var req map[string]any
	answer := lane(`{"result":{"team":"TEAM123456","teamName":"Sam","apps":[{"id":"1","name":"Intent Pal","bundleId":"com.example.intentpal","sku":"intentpal","locale":"en-GB"}]}}`, &req)
	c, f := newClient(t, func(cmd run.Cmd) (string, int, error) {
		workDir = cmd.Dir
		fastfileSeen, _ = os.ReadFile(filepath.Join(cmd.Dir, "fastlane", "Fastfile"))
		return answer(cmd)
	})
	signIn(t, c)
	res, err := c.Apps(context.Background(), "TEAM123456")
	if err != nil {
		t.Fatal(err)
	}
	if res.Team != "TEAM123456" || len(res.Apps) != 1 || res.Apps[0].BundleID != "com.example.intentpal" {
		t.Fatalf("%+v", res)
	}
	if got := f.Commands(); !slices.Equal(got, []string{"/bin/fastlane apps"}) {
		t.Fatal(got)
	}
	if !bytes.Equal(fastfileSeen, fastfile) {
		t.Fatal("fastlane didn't run the embedded Fastfile")
	}
	if req["appleId"] != "sam@example.com" || req["team"] != "TEAM123456" {
		t.Fatal(req)
	}
	cmd := f.Calls[0]
	for key, want := range map[string]string{
		"SPACESHIP_COOKIE_PATH": c.Dir, "FASTLANE_SKIP_UPDATE_CHECK": "1", "FASTLANE_OPT_OUT_USAGE": "1",
		"HOME": "/home/sam", "LANG": "en_US.UTF-8",
	} {
		if v, n := envValue(cmd, key); v != want || n != 1 {
			t.Errorf("%s=%q (%d times), want %q once", key, v, n, want)
		}
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatal("the work directory outlived the lane", err)
	}
	if fi, err := os.Stat(c.Dir); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700) {
		t.Fatal("eve's Apple directory must be 0700", fi.Mode(), err)
	}
}

func TestWithoutAnAppleIDCommandsNeedAHuman(t *testing.T) {
	c, f := newClient(t, nil)
	_, err := c.Keys(context.Background(), "")
	var ae *Error
	if !errors.As(err, &ae) || ae.ExitCode() != api.ExitNeedsHuman || !strings.Contains(err.Error(), "run `eve apple login`") {
		t.Fatal(err)
	}
	if h := ae.Human(); h == nil || h.Action != "apple_login" {
		t.Fatal(h)
	}
	if len(f.Calls) != 0 {
		t.Fatal("ran", f.Commands())
	}
}

func TestLaneErrorsMapToExitCodes(t *testing.T) {
	for kind, want := range map[string]int{
		KindSession: 78, KindAgreement: 78, KindUsage: 64, KindRejected: 65, KindUnavailable: 69, KindFailed: 1, "other": 1,
	} {
		c, _ := newClient(t, lane(`{"result":{},"error":{"kind":"`+kind+`","message":"no"}}`, nil))
		signIn(t, c)
		_, err := c.Keys(context.Background(), "")
		var ae *Error
		if !errors.As(err, &ae) || ae.ExitCode() != want {
			t.Errorf("%s: %v, want exit %d", kind, err, want)
		}
	}
	e := &Error{Kind: KindSession, Message: "no valid Apple session for sam@example.com"}
	if e.Outcome() != api.StateNeedsHuman || e.Error() != "no valid Apple session for sam@example.com; run `eve apple login`" {
		t.Fatal(e.Outcome(), e.Error())
	}
}

func TestLaneWithoutAResultFailsWithTheEndOfItsOutput(t *testing.T) {
	c, _ := newClient(t, func(run.Cmd) (string, int, error) {
		return "line 1\nline 2\n[!] Could not find a lane named apps\n", 1, nil
	})
	signIn(t, c)
	_, err := c.Apps(context.Background(), "")
	var ae *Error
	if !errors.As(err, &ae) || ae.ExitCode() != 1 || !strings.Contains(err.Error(), "Could not find a lane") {
		t.Fatal(err)
	}
}

func TestMissingFastlaneIsExplained(t *testing.T) {
	c, f := newClient(t, nil)
	signIn(t, c)
	c.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if _, err := c.Apps(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "fastlane is not on PATH") {
		t.Fatal(err)
	}
	if len(f.Calls) != 0 {
		t.Fatal(f.Commands())
	}
}

func TestLoginRemembersTheAppleIDAndKeepsTheCookiePrivate(t *testing.T) {
	c, f := newClient(t, nil)
	f.Handler = func(cmd run.Cmd) (string, int, error) {
		cookie := filepath.Join(c.Dir, "spaceship", "sam@example.com", "cookie")
		_ = os.MkdirAll(filepath.Dir(cookie), 0o755)
		_ = os.WriteFile(cookie, []byte("session"), 0o644)
		return lane(`{"result":{"signedIn":true,"team":"TEAM123456"}}`, nil)(cmd)
	}
	res, err := c.Login(context.Background(), "sam@example.com", "", "")
	if err != nil || !res.SignedIn || res.AppleID != "sam@example.com" || c.AppleID() != "sam@example.com" {
		t.Fatal(res, err)
	}
	if f.Calls[0].Stdout != nil {
		t.Fatal("login must use the terminal, not a buffer")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(c.Dir, "spaceship", "sam@example.com", "cookie"))
		if fi.Mode().Perm() != 0o600 {
			t.Fatal(fi.Mode())
		}
	}
}

// fakeDoppler answers the Doppler CLI: tokens create, secrets get (with the token it made) and
// tokens revoke. A non-zero createCode or getCode fails that step.
type fakeDoppler struct {
	createCode, getCode int
	createOut, value    string
	gotToken            []string // DOPPLER_TOKEN of each secrets get
	revoked             []string
}

func (d *fakeDoppler) answer(cmd run.Cmd) (string, int, error) {
	args := strings.Join(cmd.Args, " ")
	switch {
	case strings.HasPrefix(args, "configs tokens create"):
		if d.createOut == "" {
			d.createOut = `{"name":"eve-apple-login","token":"dp.st.prd.SECRET","slug":"slug-1"}`
		}
		return d.createOut, d.createCode, nil
	case strings.HasPrefix(args, "secrets get"):
		tok, _ := envValue(cmd, "DOPPLER_TOKEN")
		d.gotToken = append(d.gotToken, tok)
		return d.value, d.getCode, nil
	case strings.HasPrefix(args, "configs tokens revoke"):
		d.revoked = append(d.revoked, args)
		return "", 0, nil
	}
	return "", 1, nil
}

func TestLoginReadsThePasswordFromDoppler(t *testing.T) {
	c, f := newClient(t, nil)
	c.Environ = append(c.Environ, "EVE_APPLE_PASSWORD=inherited", "DOPPLER_TOKEN=inherited")
	d := &fakeDoppler{value: "hunter2 pass\n"}
	var password string
	var n int
	var laneToken int
	answer := lane(`{"result":{"signedIn":true}}`, nil)
	f.Handler = func(cmd run.Cmd) (string, int, error) {
		if strings.HasSuffix(cmd.Name, "doppler") {
			return d.answer(cmd)
		}
		password, n = envValue(cmd, "EVE_APPLE_PASSWORD")
		if v, _ := envValue(cmd, "DOPPLER_TOKEN"); v == "dp.st.prd.SECRET" {
			laneToken++
		}
		return answer(cmd)
	}
	if _, err := c.Login(context.Background(), "sam@example.com", "", "apple/prd/APPLE_ID_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	if password != "hunter2 pass" || n != 1 {
		t.Fatalf("lane got %q (%d times)", password, n)
	}
	if laneToken != 0 {
		t.Fatal("the service token reached fastlane")
	}
	cmds := f.Commands()
	want := []string{
		"/bin/doppler configs tokens create eve-apple-login --project apple --config prd --access read --max-age 1m --json",
		"/bin/doppler secrets get APPLE_ID_PASSWORD --plain",
	}
	if len(cmds) < 2 || cmds[0] != want[0] || cmds[1] != want[1] {
		t.Fatal(cmds)
	}
	// Read with the new token alone, then revoked.
	if len(d.gotToken) != 1 || d.gotToken[0] != "dp.st.prd.SECRET" {
		t.Fatal("secrets get used", d.gotToken)
	}
	for _, cmd := range f.Calls {
		if strings.Join(cmd.Args, " ") == "secrets get APPLE_ID_PASSWORD --plain" {
			if _, k := envValue(cmd, "DOPPLER_TOKEN"); k != 1 {
				t.Fatal("DOPPLER_TOKEN set", k, "times")
			}
		}
	}
	if len(d.revoked) != 1 || d.revoked[0] != "configs tokens revoke --slug slug-1 --project apple --config prd --silent" {
		t.Fatal("revoked", d.revoked)
	}
	for _, cmd := range cmds {
		if strings.Contains(cmd, "hunter2") || strings.Contains(cmd, "dp.st.prd.SECRET") {
			t.Fatal("secret in argv:", cmd)
		}
	}
}

func TestLoginWithoutDopplerAndOtherLanesNeverInheritAPassword(t *testing.T) {
	c, f := newClient(t, lane(`{"result":{}}`, nil))
	c.Environ = append(c.Environ, "EVE_APPLE_PASSWORD=inherited")
	if _, err := c.Login(context.Background(), "sam@example.com", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Keys(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range f.Calls {
		if _, n := envValue(cmd, "EVE_APPLE_PASSWORD"); n != 0 {
			t.Fatal("inherited password reached", cmd.Args)
		}
	}
}

func TestLoginDopplerFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		ref     string
		d       fakeDoppler
		kind    string
		inMsg   string
		revoked int
	}{
		"bad ref":     {ref: "apple/APPLE_ID_PASSWORD", kind: KindUsage, inMsg: "<project>/<config>/<SECRET>"},
		"bad secret":  {ref: "apple/prd/apple-pw", kind: KindUsage, inMsg: "<project>/<config>/<SECRET>"},
		"no token":    {ref: "apple/prd/PW", d: fakeDoppler{createCode: 1}, kind: KindFailed, inMsg: "signed in?"},
		"odd token":   {ref: "apple/prd/PW", d: fakeDoppler{createOut: "{}"}, kind: KindFailed, inMsg: "read token"},
		"read failed": {ref: "apple/prd/PW", d: fakeDoppler{getCode: 1}, kind: KindFailed, inMsg: "couldn't read", revoked: 1},
		"empty value": {ref: "apple/prd/PW", d: fakeDoppler{value: "\n"}, kind: KindUsage, inMsg: "is empty", revoked: 1},
	} {
		t.Run(name, func(t *testing.T) {
			d := tc.d
			c, f := newClient(t, d.answer)
			_, err := c.Login(context.Background(), "sam@example.com", "", tc.ref)
			var ae *Error
			if !errors.As(err, &ae) || ae.Kind != tc.kind || !strings.Contains(ae.Message, tc.inMsg) {
				t.Fatal(err)
			}
			if len(d.revoked) != tc.revoked {
				t.Fatal("revoked", d.revoked)
			}
			for _, cmd := range f.Commands() {
				if strings.Contains(cmd, "fastlane") {
					t.Fatal("ran fastlane after Doppler failed")
				}
			}
		})
	}
}

func TestAppSpecValidation(t *testing.T) {
	ok := AppSpec{BundleID: "com.example.intentpal", Name: "Intent Pal", SKU: "intentpal", Locale: "en-GB", Version: "1.0"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*AppSpec){
		"bundle id":    func(s *AppSpec) { s.BundleID = "intentpal" },
		"sku":          func(s *AppSpec) { s.SKU = "intent pal" },
		"locale":       func(s *AppSpec) { s.Locale = "English" },
		"version":      func(s *AppSpec) { s.Version = "v1" },
		"like itself":  func(s *AppSpec) { s.Like = s.BundleID },
		"like":         func(s *AppSpec) { s.Like = "screengate" },
		"extension":    func(s *AppSpec) { s.Extensions = []string{"a.b"} },
		"capability":   func(s *AppSpec) { s.Capabilities = []string{"push"} },
		"ext cap":      func(s *AppSpec) { s.ExtensionCapabilities = []string{"App Groups"} },
		"tester alone": func(s *AppSpec) { s.Tester = "sam@example.com" },
		"tester":       func(s *AppSpec) { s.Group, s.Tester = "Internal", "sam" },
	} {
		s := ok
		mutate(&s)
		var ae *Error
		if err := s.Validate(); !errors.As(err, &ae) || ae.ExitCode() != api.ExitUsage {
			t.Errorf("%s: %v", name, err)
		}
	}
	existing := AppSpec{BundleID: "com.example.intentpal", Locale: "en-US", Version: "1.0", Group: "Internal", Tester: "sam@example.com"}
	if err := existing.Validate(); err != nil {
		t.Fatal("an existing record needs no --name or --sku:", err)
	}
	s := ok
	s.Capabilities = []string{"FAMILY_CONTROLS_DISTRIBUTION", "PUSH_NOTIFICATIONS"}
	if err := s.Validate(); err != nil {
		t.Fatal("Family Controls (Distribution) is an ordinary capability:", err)
	}
}

func TestCreateAppSendsTheSpecAndKeepsPartialChanges(t *testing.T) {
	var req map[string]any
	c, f := newClient(t, lane(`{"result":{"team":"TEAM123456","bundleId":"com.example.intentpal","changes":["created app group group.com.example.intentpal"]},"error":{"kind":"rejected","message":"App Store Connect refused the app record: name taken"}}`, &req))
	signIn(t, c)
	res, err := c.CreateApp(context.Background(), "", AppSpec{
		BundleID: "com.example.intentpal", Name: "Intent Pal", SKU: "intentpal", Locale: "en-GB", Version: "1.0",
		Like: "com.example.screengate", Extensions: []string{"monitor", "shield"},
		Group: "Internal", Tester: "sam@example.com", DryRun: true,
	})
	var ae *Error
	if !errors.As(err, &ae) || ae.ExitCode() != api.ExitRejected {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || !res.DryRun {
		t.Fatalf("%+v", res)
	}
	if got := f.Commands(); !slices.Equal(got, []string{"/bin/fastlane app_create"}) {
		t.Fatal(got)
	}
	exts, _ := json.Marshal(req["extensions"])
	caps, _ := json.Marshal(req["capabilities"])
	if req["like"] != "com.example.screengate" || string(exts) != `["monitor","shield"]` || string(caps) != `[]` ||
		req["dryRun"] != true || req["locale"] != "en-GB" || req["team"] != "" ||
		req["group"] != "Internal" || req["tester"] != "sam@example.com" {
		t.Fatal(req)
	}
}

func TestKeySpecValidationAndDopplerTargets(t *testing.T) {
	for name, s := range map[string]KeySpec{
		"parentheses":    {Name: "Intent Pal (prod)", Env: "production", Scope: "team", Doppler: []string{"intentpal"}},
		"env":            {Name: "Push", Env: "development", Scope: "team", Doppler: []string{"intentpal"}},
		"scope":          {Name: "Push", Env: "sandbox", Scope: "app", Doppler: []string{"intentpal"}},
		"topic no ids":   {Name: "Push", Env: "sandbox", Scope: "topic", Doppler: []string{"intentpal"}},
		"topic all envs": {Name: "Push", Env: "all", Scope: "topic", Topics: []string{"com.example.a"}, Doppler: []string{"intentpal"}},
		"team topics":    {Name: "Push", Env: "all", Scope: "team", Topics: []string{"com.example.a"}, Doppler: []string{"intentpal"}},
		"bad topic":      {Name: "Push", Env: "sandbox", Scope: "topic", Topics: []string{"a"}, Doppler: []string{"intentpal"}},
		"no doppler":     {Name: "Push", Env: "all", Scope: "team"},
		"bad doppler":    {Name: "Push", Env: "all", Scope: "team", Doppler: []string{"intent pal/dev"}},
		"empty config":   {Name: "Push", Env: "all", Scope: "team", Doppler: []string{"intentpal/"}},
	} {
		var ae *Error
		if err := s.Validate(); !errors.As(err, &ae) || ae.ExitCode() != api.ExitUsage {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, tc := range []struct {
		env     string
		doppler []string
		want    []string
	}{
		{"sandbox", []string{"intentpal"}, []string{"intentpal/dev"}},
		{"production", []string{"intentpal"}, []string{"intentpal/prd"}},
		{"all", []string{"intentpal"}, []string{"intentpal/dev", "intentpal/prd"}},
		{"all", []string{"intentpal/stg", "apple", "intentpal/stg"}, []string{"intentpal/stg", "apple/dev", "apple/prd"}},
	} {
		s := KeySpec{Name: "Intent Pal Push", Env: tc.env, Scope: "team", Doppler: tc.doppler}
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
		ts, _ := s.targets()
		var got []string
		for _, t := range ts {
			got = append(got, t.String())
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s %v: %v, want %v", tc.env, tc.doppler, got, tc.want)
		}
	}
}

// keyFake acts as fastlane (creating the key and its rescue file), and as the Doppler CLI with
// the projects listed and secrets set to fail for failSet.
type keyFake struct {
	c        *Client
	projects string
	failSet  string
	req      map[string]any
	stdin    map[string][]byte // "<config> <name>" -> value
}

func (k *keyFake) handle(cmd run.Cmd) (string, int, error) {
	args := strings.Join(cmd.Args, " ")
	switch {
	case strings.HasSuffix(cmd.Name, "fastlane"):
		path := filepath.Join(k.c.RescueDir, "eve-AuthKey_KEY1234567.p8")
		if err := os.WriteFile(path, []byte(p8), 0o600); err != nil {
			return "", 1, err
		}
		result, _ := json.Marshal(map[string]any{"result": map[string]any{"team": "TEAM123456", "keyId": "KEY1234567", "rescuePath": path}})
		return lane(string(result), &k.req)(cmd)
	case args == "projects --json":
		return k.projects, 0, nil
	case strings.HasPrefix(args, "projects create"):
		return "", 0, nil
	case strings.HasPrefix(args, "configs --project"):
		return `[{"name":"dev"},{"name":"stg"},{"name":"prd"}]`, 0, nil
	case strings.HasPrefix(args, "secrets set"):
		value, _ := io.ReadAll(cmd.Stdin)
		if k.stdin == nil {
			k.stdin = map[string][]byte{}
		}
		k.stdin[cmd.Args[6]+" "+cmd.Args[2]] = value
		if cmd.Args[2] == k.failSet {
			_, _ = io.WriteString(cmd.Stderr, "Doppler Error: could not save "+p8)
			return "", 1, nil
		}
		// Doppler's stdout can echo values; it must go nowhere.
		if cmd.Stdout != io.Discard {
			return "", 1, errors.New("stdout not discarded")
		}
		return "", 0, nil
	}
	return "", 1, errors.New("unexpected command " + args)
}

func TestCreateKeyStoresEverySecretInDopplerOnStdin(t *testing.T) {
	c, f := newClient(t, nil)
	k := &keyFake{c: c, projects: `[{"name":"apple"}]`}
	f.Handler = k.handle
	signIn(t, c)
	res, err := c.CreateKey(context.Background(), "", KeySpec{Name: "Intent Pal Push", Env: "all", Scope: "team", Doppler: []string{"intentpal"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/bin/doppler projects --json",
		"/bin/doppler projects create intentpal --silent --description Created by eve apple key create",
		"/bin/doppler configs --project intentpal --json",
		"/bin/fastlane apns_key",
		"/bin/doppler secrets set APNS_KEY_P8 --project intentpal --config dev --no-interactive --silent --visibility restricted",
		"/bin/doppler secrets set APNS_KEY_ID --project intentpal --config dev --no-interactive --silent",
		"/bin/doppler secrets set APPLE_TEAM_ID --project intentpal --config dev --no-interactive --silent",
		"/bin/doppler secrets set APNS_KEY_P8 --project intentpal --config prd --no-interactive --silent --visibility restricted",
		"/bin/doppler secrets set APNS_KEY_ID --project intentpal --config prd --no-interactive --silent",
		"/bin/doppler secrets set APPLE_TEAM_ID --project intentpal --config prd --no-interactive --silent",
	}
	if got := f.Commands(); !slices.Equal(got, want) {
		t.Fatalf("got\n%s", strings.Join(got, "\n"))
	}
	if k.req["environment"] != "all" || k.req["scope"] != "team" || k.req["name"] != "Intent Pal Push" || k.req["rescueDir"] != c.RescueDir {
		t.Fatal(k.req)
	}
	for _, config := range []string{"dev", "prd"} {
		if string(k.stdin[config+" APNS_KEY_P8"]) != p8 || string(k.stdin[config+" APNS_KEY_ID"]) != "KEY1234567" || string(k.stdin[config+" APPLE_TEAM_ID"]) != "TEAM123456" {
			t.Fatalf("%s: stdin %q", config, k.stdin)
		}
	}
	for _, cmd := range f.Commands() {
		if strings.Contains(cmd, "PRIVATE KEY") || strings.Contains(cmd, "SECRET") {
			t.Fatal("a value reached argv:", cmd)
		}
	}
	if !slices.Equal(res.Stored, []string{"intentpal/dev", "intentpal/prd"}) || !slices.Equal(res.CreatedProjects, []string{"intentpal"}) ||
		res.KeyID != "KEY1234567" || res.RescuePath != "" {
		t.Fatalf("%+v", res)
	}
	if entries, _ := os.ReadDir(c.RescueDir); len(entries) != 0 {
		t.Fatal("the rescue copy outlived a successful store", entries)
	}
}

func TestCreateKeyMapsEnvironmentsToApplesValues(t *testing.T) {
	for env, want := range map[string]string{"sandbox": "sandbox", "production": "prod"} {
		c, f := newClient(t, nil)
		k := &keyFake{c: c, projects: `[{"name":"intentpal"}]`}
		f.Handler = k.handle
		signIn(t, c)
		_, err := c.CreateKey(context.Background(), "TEAM123456", KeySpec{
			Name: "Push", Env: env, Scope: "topic", Topics: []string{"com.example.intentpal"}, Doppler: []string{"intentpal"},
		})
		if err != nil {
			t.Fatal(err)
		}
		topics, _ := json.Marshal(k.req["topics"])
		if k.req["environment"] != want || k.req["scope"] != "topic" || string(topics) != `["com.example.intentpal"]` {
			t.Fatal(env, k.req)
		}
	}
}

func TestCreateKeyKeepsTheRescueCopyWhenStoringFails(t *testing.T) {
	c, f := newClient(t, nil)
	k := &keyFake{c: c, projects: `[{"name":"intentpal"}]`, failSet: "APNS_KEY_ID"}
	f.Handler = k.handle
	signIn(t, c)
	res, err := c.CreateKey(context.Background(), "", KeySpec{Name: "Push", Env: "production", Scope: "team", Doppler: []string{"intentpal"}})
	var ae *Error
	if !errors.As(err, &ae) || ae.ExitCode() != api.ExitFailed {
		t.Fatal(err)
	}
	path := filepath.Join(c.RescueDir, "eve-AuthKey_KEY1234567.p8")
	if res.RescuePath != path || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "APNS_KEY_ID in intentpal/prd") {
		t.Fatal(res.RescuePath, err)
	}
	if strings.Contains(err.Error(), "SECRETSECRET") || strings.Contains(err.Error(), "MIGTAgEA") {
		t.Fatal("key material in the error:", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != p8 {
		t.Fatal("rescue copy lost", err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
}

func TestCreateKeyChecksDopplerBeforeCreatingAKey(t *testing.T) {
	// A config the project lacks.
	c, f := newClient(t, nil)
	k := &keyFake{c: c, projects: `[{"name":"intentpal"}]`}
	f.Handler = k.handle
	signIn(t, c)
	_, err := c.CreateKey(context.Background(), "", KeySpec{Name: "Push", Env: "all", Scope: "team", Doppler: []string{"intentpal/staging"}})
	var ae *Error
	if !errors.As(err, &ae) || ae.ExitCode() != api.ExitUsage || !strings.Contains(err.Error(), "no config staging (it has dev, stg, prd)") {
		t.Fatal(err)
	}
	// Doppler not signed in.
	c2, f2 := newClient(t, func(cmd run.Cmd) (string, int, error) {
		_, _ = io.WriteString(cmd.Stderr, "Doppler Error: you must provide a token")
		return "", 1, nil
	})
	signIn(t, c2)
	_, err = c2.CreateKey(context.Background(), "", KeySpec{Name: "Push", Env: "all", Scope: "team", Doppler: []string{"intentpal"}})
	if err == nil || !strings.Contains(err.Error(), "doppler login") {
		t.Fatal(err)
	}
	for _, fake := range []*run.Fake{f, f2} {
		for _, cmd := range fake.Commands() {
			if strings.Contains(cmd, "fastlane") {
				t.Fatal("created a key that couldn't be stored")
			}
		}
	}
	// Doppler missing.
	c3, f3 := newClient(t, nil)
	signIn(t, c3)
	c3.LookPath = func(name string) (string, error) {
		if name == "doppler" {
			return "", errors.New("not found")
		}
		return "/bin/" + name, nil
	}
	if _, err := c3.CreateKey(context.Background(), "", KeySpec{Name: "Push", Env: "all", Scope: "team", Doppler: []string{"intentpal"}}); err == nil || len(f3.Calls) != 0 {
		t.Fatal(err, f3.Commands())
	}
}
