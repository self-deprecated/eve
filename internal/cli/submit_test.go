package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/client"
)

// fakeServer answers the version and apps endpoints only, recording every request.
func fakeServer(t *testing.T, apiLevel int) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/version":
			_ = json.NewEncoder(w).Encode(api.VersionInfo{Version: "0.1.0", API: apiLevel})
		case "/v1/apps":
			_ = json.NewEncoder(w).Encode([]api.App{{Name: "screengate", Verbs: []api.Verb{api.VerbTestflight, api.VerbCheck}}})
		default:
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

func submitIn(t *testing.T, srv *httptest.Server, verb string) error {
	t.Helper()
	dir := t.TempDir()
	yml := "app: screengate\nproject: {xcodeproj: ScreenGate.xcodeproj, scheme: ScreenGate}\n"
	if err := os.WriteFile(filepath.Join(dir, "eve.yml"), []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	root, env := Root("test")
	env.Out, env.Err = &bytes.Buffer{}, &bytes.Buffer{}
	env.NewClient = func() (*client.Client, error) { return client.New(srv.URL, "tok"), nil }
	root.SetArgs([]string{"submit", verb, "--wait"})
	return root.Execute()
}

func TestSubmitRefusesUnavailableVerbBeforeUploading(t *testing.T) {
	srv, paths := fakeServer(t, api.APILevel)
	err := submitIn(t, srv, "device")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != api.ExitUsage || !strings.Contains(ee.Error(), "available: testflight, check") {
		t.Fatalf("err %v", err)
	}
	if strings.Join(*paths, ", ") != "GET /v1/version, GET /v1/apps" {
		t.Fatalf("requests %v: only the version and apps list may be fetched", *paths)
	}
}

func TestSubmitRefusesAnOlderServerBeforeUploading(t *testing.T) {
	srv, paths := fakeServer(t, 0) // a server from before API levels reports none
	err := submitIn(t, srv, "testflight")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != api.ExitUnavailable || !strings.Contains(ee.Error(), "older than this client (API level 0, needs") {
		t.Fatalf("err %v", err)
	}
	if strings.Join(*paths, ", ") != "GET /v1/version" {
		t.Fatalf("requests %v", *paths)
	}
}
