package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/self-deprecated/eve/internal/config"
)

func TestInitFindsProjectOneLevelDown(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ios", "ScreenGate.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	root, env := Root("test")
	env.Out = &bytes.Buffer{}
	root.SetArgs([]string{"init"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	repo, err := config.LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if repo.App != "screengate" || repo.Project.Dir != "ios" || repo.Project.Xcodeproj != "ScreenGate.xcodeproj" || repo.Project.Scheme != "ScreenGate" {
		t.Fatalf("%+v", repo)
	}
	root.SetArgs([]string{"init"})
	if err := root.Execute(); err == nil {
		t.Fatal("init overwrote an existing eve.yml")
	}
}
