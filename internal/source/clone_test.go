package source

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestClone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cloning workspaces happens on the build Mac")
	}
	tests := []struct {
		name  string
		clone func(src, dst string) error
	}{
		{"Clone", Clone},
		{"copy fallback", copyTree},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := t.TempDir()
			writeTree(t, src, map[string]string{
				"ios/project.yml":      "name: Driver\n",
				"ios/Driver/App.swift": "import SwiftUI\n",
				"scripts/run.sh*":      "#!/bin/sh\n",
				"ios/Shared":           "->Driver",
			})
			dst := filepath.Join(t.TempDir(), "ws")
			if err := tt.clone(src, dst); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(dst, "ios/project.yml")); err != nil || string(b) != "name: Driver\n" {
				t.Errorf("content: %q %v", b, err)
			}
			if fi, err := os.Stat(filepath.Join(dst, "scripts/run.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
				t.Errorf("exec bit lost: %v %v", fi, err)
			}
			if l, err := os.Readlink(filepath.Join(dst, "ios/Shared")); err != nil || l != "Driver" {
				t.Errorf("symlink: %q %v", l, err)
			}
			// Writing to the clone leaves the source alone.
			if err := os.WriteFile(filepath.Join(dst, "ios/project.yml"), []byte("changed"), 0o644); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(filepath.Join(src, "ios/project.yml")); string(b) != "name: Driver\n" {
				t.Error("source changed through the clone")
			}
		})
	}
}

func TestCloneRefusesExistingDest(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := Clone(src, dst); err == nil {
		t.Error("cloned over an existing directory")
	}
	f := filepath.Join(src, "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Clone(f, filepath.Join(dst, "x")); err == nil {
		t.Error("cloned a regular file")
	}
}
