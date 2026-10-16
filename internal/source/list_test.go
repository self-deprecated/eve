package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// writeTree creates files under root; a value starting with "->" makes a symlink, a key ending
// in "*" an executable file.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		mode := os.FileMode(0o644)
		if rel[len(rel)-1] == '*' {
			rel, mode = rel[:len(rel)-1], 0o755
		}
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if len(content) > 2 && content[:2] == "->" {
			if err := os.Symlink(content[2:], p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func rels(fs []File) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Rel)
	}
	return out
}

// driverLikeTree mirrors ~/Developer/driver, which is not a git repository: ios/.gitignore
// excludes build/ and *.xcodeproj/.
func driverLikeTree(t *testing.T) string {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":                         "*.log\n",
		".eveignore":                         "# eve only\n/docs/\n",
		"ios/.gitignore":                     "build/\n*.xcodeproj/\n",
		"ios/project.yml":                    "name: Driver\n",
		"ios/README.md":                      "# Driver\n",
		"ios/Driver/App.swift":               "import SwiftUI\n",
		"ios/Driver/Info.plist":              "<plist/>\n",
		"ios/Widgets/Widgets.swift":          "import WidgetKit\n",
		"ios/build/testflight/Driver.ipa":    "ipa",
		"ios/Driver.xcodeproj/project.pbxpr": "pbx",
		"scripts/deploy.sh*":                 "#!/bin/sh\n",
		"scripts/.gitignore":                 "!keep.log\n",
		"scripts/keep.log":                   "kept",
		"scripts/run.log":                    "dropped",
		"scripts/scratch.tmp":                "dropped by extraIgnore",
		"docs/plan.md":                       "dropped by .eveignore",
		"node_modules/left-pad/index.js":     "module.exports = 1\n",
		"node_modules/.gitignore":            "*.map\n",
		"node_modules/left-pad/index.js.map": "{}",
		".env":                               "SECRET=1\n",
		"ios/AuthKey_ABC123.p8":              "-----BEGIN PRIVATE KEY-----",
		"ios/.DS_Store":                      "\x00",
		"ios/Shared":                         "->Driver",
	})
	return root
}

func TestListWalk(t *testing.T) {
	root := driverLikeTree(t)
	res, err := List(root, []string{"*.tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Git {
		t.Fatal("temp tree listed through git")
	}
	wantFiles := []string{
		".eveignore", ".gitignore",
		"ios/.gitignore", "ios/Driver/App.swift", "ios/Driver/Info.plist", "ios/README.md",
		"ios/Shared", "ios/Widgets/Widgets.swift", "ios/project.yml",
		"scripts/.gitignore", "scripts/deploy.sh", "scripts/keep.log",
	}
	if got := rels(res.Files); !reflect.DeepEqual(got, wantFiles) {
		t.Errorf("Files:\n got %q\nwant %q", got, wantFiles)
	}
	wantExcluded := []string{
		".env", "ios/.DS_Store", "ios/AuthKey_ABC123.p8",
		"node_modules/.gitignore", "node_modules/left-pad/index.js",
	}
	if got := rels(res.Excluded); !reflect.DeepEqual(got, wantExcluded) {
		t.Errorf("Excluded:\n got %q\nwant %q", got, wantExcluded)
	}
	for _, f := range res.Files {
		switch f.Rel {
		case "ios/Shared":
			if f.Link != "Driver" || f.Mode&os.ModeSymlink == 0 {
				t.Errorf("symlink recorded as %+v", f)
			}
		case "scripts/deploy.sh":
			if f.Mode&0o111 == 0 {
				t.Errorf("deploy.sh lost its exec bit: %v", f.Mode)
			}
		case "ios/project.yml":
			if f.Size != int64(len("name: Driver\n")) {
				t.Errorf("project.yml size %d", f.Size)
			}
		}
	}
}

func TestStats(t *testing.T) {
	st := Stats([]File{
		{Rel: "ios/a.swift", Size: 10},
		{Rel: "ios/b/c.swift", Size: 5},
		{Rel: "README.md", Size: 3},
	})
	if st.Bytes != 18 || st.Files != 3 {
		t.Errorf("totals %+v", st)
	}
	if want := map[string]int64{"ios": 15, "README.md": 3}; !reflect.DeepEqual(st.TopDir, want) {
		t.Errorf("TopDir %v, want %v", st.TopDir, want)
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=eve", "-c", "user.email=eve@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestListGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":           "ignored.txt\nbuild/\n",
		".eveignore":           "docs/\n",
		"tracked.swift":        "let a = 1\n",
		"gone.swift":           "deleted from the work tree below\n",
		"forced.log":           "tracked although ignored\n",
		"ignored.txt":          "git ignores me",
		"untracked.swift":      "let b = 2\n",
		"docs/guide.md":        "dropped by .eveignore",
		"node_modules/x/i.js":  "excluded, but git would upload it",
		"build/out.o":          "gitignored",
		"Config/Secrets.p12":   "excluded secret",
		"Sources/App/App.mm":   "objc",
		"Sources/App/Link.txt": "->App.mm",
	})
	gitCmd(t, root, "init", "-q", "-b", "main")
	gitCmd(t, root, "add", ".gitignore", "tracked.swift", "gone.swift")
	gitCmd(t, root, "add", "-f", "forced.log")
	gitCmd(t, root, "commit", "-q", "-m", "init")
	if err := os.Remove(filepath.Join(root, "gone.swift")); err != nil {
		t.Fatal(err)
	}

	res, err := List(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Git {
		t.Fatal("expected a git listing")
	}
	wantFiles := []string{
		".eveignore", ".gitignore", "Sources/App/App.mm", "Sources/App/Link.txt",
		"forced.log", "tracked.swift", "untracked.swift",
	}
	if got := rels(res.Files); !reflect.DeepEqual(got, wantFiles) {
		t.Errorf("Files:\n got %q\nwant %q", got, wantFiles)
	}
	wantExcluded := []string{"Config/Secrets.p12", "node_modules/x/i.js"}
	if got := rels(res.Excluded); !reflect.DeepEqual(got, wantExcluded) {
		t.Errorf("Excluded:\n got %q\nwant %q", got, wantExcluded)
	}

	commit, branch, dirty, ok := GitInfo(root)
	if !ok || len(commit) != 40 || branch != "main" || !dirty {
		t.Errorf("GitInfo = %q %q %v %v", commit, branch, dirty, ok)
	}
}

func TestGitInfoOutsideRepo(t *testing.T) {
	if _, _, _, ok := GitInfo(t.TempDir()); ok {
		t.Error("GitInfo reported a repository in an empty temp dir")
	}
}
