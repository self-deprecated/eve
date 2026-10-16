package source

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func packTree(t *testing.T, root string) (PackInfo, []byte) {
	t.Helper()
	res, err := List(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	info, err := Pack(root, res.Files, &buf)
	if err != nil {
		t.Fatal(err)
	}
	return info, buf.Bytes()
}

func TestPackDeterministic(t *testing.T) {
	files := map[string]string{
		"ios/project.yml":      "name: Driver\n",
		"ios/Driver/App.swift": "import SwiftUI\n",
		"scripts/run.sh*":      "#!/bin/sh\n",
		"ios/Shared":           "->Driver",
		"README.md":            "hello",
	}
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, files)
	writeTree(t, b, files)
	// Different times, owners' umask-ish modes and creation order must not matter.
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(b, "README.md"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(b, "ios/project.yml"), 0o600); err != nil {
		t.Fatal(err)
	}

	ia, da := packTree(t, a)
	ib, db := packTree(t, b)
	ia2, _ := packTree(t, a)
	if ia.SHA256 != ib.SHA256 || ia.SHA256 != ia2.SHA256 {
		t.Fatalf("digests differ: %s %s %s", ia.SHA256, ib.SHA256, ia2.SHA256)
	}
	sum := sha256.Sum256(da)
	if hex.EncodeToString(sum[:]) != ia.SHA256 || int64(len(da)) != ia.Bytes {
		t.Errorf("PackInfo does not describe the written bytes")
	}
	if !bytes.Equal(da, db) {
		t.Error("archives differ")
	}
	if ia.Stats.Files != 5 || ia.Stats.TopDir["ios"] != int64(len("name: Driver\n")+len("import SwiftUI\n")) {
		t.Errorf("stats %+v", ia.Stats)
	}

	// Any content change changes the digest.
	writeTree(t, b, map[string]string{"README.md": "hellO"})
	if ic, _ := packTree(t, b); ic.SHA256 == ia.SHA256 {
		t.Error("content change kept the digest")
	}
}

func TestPackRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{
		"ios/Driver/App.swift":  "import SwiftUI\n",
		"ios/Driver/Big.bin":    strings.Repeat("x", 300_000),
		"scripts/run.sh*":       "#!/bin/sh\necho hi\n",
		"ios/Shared":            "->Driver",
		"ios/Driver/Up":         "->../project.yml",
		"ios/project.yml":       "name: Driver\n",
		"empty.txt":             "",
		"deep/a/b/c/d/e/f.json": "{}",
	})
	info, data := packTree(t, src)
	dest := filepath.Join(t.TempDir(), "ws")
	if err := Extract(bytes.NewReader(data), dest, Limits{}); err != nil {
		t.Fatal(err)
	}
	// Re-packing the extracted tree reproduces the archive exactly.
	if again, _ := packTree(t, dest); again.SHA256 != info.SHA256 {
		t.Errorf("round trip changed the digest")
	}
	if fi, err := os.Stat(filepath.Join(dest, "scripts/run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode: %v %v", fi, err)
	}
	if link, err := os.Readlink(filepath.Join(dest, "ios/Shared")); err != nil || link != "Driver" {
		t.Errorf("symlink: %q %v", link, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "ios/Driver/Up")); err != nil || string(b) != "name: Driver\n" {
		t.Errorf("relative symlink content: %q %v", b, err)
	}
}

func TestPackRejectsEscapingSymlink(t *testing.T) {
	for _, target := range []string{"/etc/passwd", "../outside", "a/../../x"} {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"link": "->" + target, "a/f": "x"})
		files := []File{{Rel: "a/f"}, {Rel: "link"}}
		if _, err := Pack(root, files, &bytes.Buffer{}); err == nil {
			t.Errorf("symlink to %q was packed", target)
		}
	}
}

func TestPackRejectsVanishedFile(t *testing.T) {
	root := t.TempDir()
	if _, err := Pack(root, []File{{Rel: "missing.txt"}}, &bytes.Buffer{}); err == nil {
		t.Error("missing file packed")
	}
	if _, err := Pack(root, []File{{Rel: "../x"}}, &bytes.Buffer{}); err == nil {
		t.Error("escaping path packed")
	}
}
