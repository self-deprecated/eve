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

func TestSubmitRefusesUnavailableVerbBeforeUploading(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/v1/apps" {
			_ = json.NewEncoder(w).Encode([]api.App{{Name: "screengate", Verbs: []api.Verb{api.VerbTestflight, api.VerbCheck}}})
			return
		}
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer srv.Close()

	dir := t.TempDir()
	yml := "app: screengate\nproject: {xcodeproj: ScreenGate.xcodeproj, scheme: ScreenGate}\n"
	if err := os.WriteFile(filepath.Join(dir, "eve.yml"), []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	root, env := Root("test")
	var stderr bytes.Buffer
	env.Out, env.Err = &bytes.Buffer{}, &stderr
	env.NewClient = func() (*client.Client, error) { return client.New(srv.URL, "tok"), nil }
	root.SetArgs([]string{"submit", "device", "--wait"})
	err := root.Execute()

	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != api.ExitUsage || !strings.Contains(ee.Error(), "available: testflight, check") {
		t.Fatalf("err %v", err)
	}
	if len(paths) != 1 || paths[0] != "GET /v1/apps" {
		t.Fatalf("requests %v: only the apps list may be fetched", paths)
	}
}
