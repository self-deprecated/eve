package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/client"
)

func tarOf(t *testing.T, files map[string]string, extra ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "dark/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	for _, h := range extra {
		_ = tw.WriteHeader(h)
	}
	_ = tw.Close()
	return b.Bytes()
}

func artifactServer(t *testing.T, current []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/jobs/j_1":
			_ = json.NewEncoder(w).Encode(api.Result{ID: "j_1", Artifacts: []string{"current/", "report.txt"}})
		case "/v1/jobs/j_1/artifacts/current":
			_, _ = w.Write(current)
		case "/v1/jobs/j_1/artifacts/report.txt":
			_, _ = w.Write([]byte("2 screens"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runArtifacts(t *testing.T, srv *httptest.Server, out string) error {
	t.Helper()
	root, env := Root("test")
	env.Out, env.Err = &bytes.Buffer{}, &bytes.Buffer{}
	env.NewClient = func() (*client.Client, error) { return client.New(srv.URL, "tok"), nil }
	root.SetArgs([]string{"artifacts", "j_1", "--out", out})
	return root.Execute()
}

func TestArtifactsReplacesDirectoriesAndWritesFiles(t *testing.T) {
	out := t.TempDir()
	_ = os.MkdirAll(filepath.Join(out, "current"), 0o755)
	_ = os.WriteFile(filepath.Join(out, "current", "stale.png"), []byte("old"), 0o644)
	_ = os.WriteFile(filepath.Join(out, "keep.md"), []byte("mine"), 0o644)

	srv := artifactServer(t, tarOf(t, map[string]string{"01-home.png": "png", "dark/01-home.png": "dark"}))
	if err := runArtifacts(t, srv, out); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"current/01-home.png": "png", "current/dark/01-home.png": "dark", "report.txt": "2 screens", "keep.md": "mine"} {
		if b, err := os.ReadFile(filepath.Join(out, path)); err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", path, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "current", "stale.png")); err == nil {
		t.Fatal("a directory artifact replaces the old directory")
	}
}

func TestArtifactsRefusesPathsOutsideTheDirectory(t *testing.T) {
	out := t.TempDir()
	_ = os.MkdirAll(filepath.Join(out, "current"), 0o755)
	_ = os.WriteFile(filepath.Join(out, "current", "old.png"), []byte("old"), 0o644)
	evil := tarOf(t, nil, &tar.Header{Name: "../../escape", Typeflag: tar.TypeReg, Mode: 0o644})
	if err := runArtifacts(t, artifactServer(t, evil), out); err == nil {
		t.Fatal("an escaping path was accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(out, "current", "old.png")); string(b) != "old" {
		t.Fatal("a failed download must leave the old directory alone")
	}
}
