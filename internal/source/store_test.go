package source

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestStorePutHasOpen(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "sources")}
	data := []byte("archive bytes")
	sha := digest(data)
	if s.Has(sha) {
		t.Fatal("empty store has the archive")
	}
	if err := s.Put(sha, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if !s.Has(sha) {
		t.Fatal("stored archive missing")
	}
	fi, err := os.Stat(s.Path(sha))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("stored mode %v, %v", fi.Mode(), err)
	}
	f, err := s.Open(sha)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if got, _ := io.ReadAll(f); !bytes.Equal(got, data) {
		t.Errorf("read back %q", got)
	}
	// Re-uploading the same content is harmless.
	if err := s.Put(sha, bytes.NewReader(data)); err != nil {
		t.Error(err)
	}
}

func TestStoreRejects(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	sha := digest([]byte("expected"))
	if err := s.Put(sha, bytes.NewReader([]byte("tampered"))); !errors.Is(err, ErrBadDigest) {
		t.Errorf("mismatch: %v", err)
	}
	if s.Has(sha) {
		t.Error("mismatched upload stored")
	}
	for _, bad := range []string{"", "../../etc/passwd", sha[:63], "ABCDEF" + sha[6:]} {
		if err := s.Put(bad, bytes.NewReader(nil)); !errors.Is(err, ErrBadDigest) {
			t.Errorf("Put(%q) = %v", bad, err)
		}
		if s.Has(bad) {
			t.Errorf("Has(%q)", bad)
		}
		if _, err := s.Open(bad); err == nil {
			t.Errorf("Open(%q) succeeded", bad)
		}
	}
	entries, _ := os.ReadDir(s.Dir)
	if len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestStorePrune(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	oldData, newData := []byte("old"), []byte("new")
	for _, d := range [][]byte{oldData, newData} {
		if err := s.Put(digest(d), bytes.NewReader(d)); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(s.Path(digest(oldData)), stale, stale); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(s.Dir, tempPrefix+"abandoned")
	unrelated := filepath.Join(s.Dir, "notes.txt")
	for _, p := range []string{tmp, unrelated} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Prune(7 * 24 * time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v; want 2", n, err)
	}
	if s.Has(digest(oldData)) || !s.Has(digest(newData)) {
		t.Error("pruned the wrong archive")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Error("pruned a file the store does not own")
	}
	if n, err := (Store{Dir: filepath.Join(s.Dir, "missing")}).Prune(time.Hour); n != 0 || err != nil {
		t.Errorf("missing dir: %d %v", n, err)
	}
}

func TestStoreHasRefreshesTime(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	d := []byte("reused")
	if err := s.Put(digest(d), bytes.NewReader(d)); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(s.Path(digest(d)), stale, stale); err != nil {
		t.Fatal(err)
	}
	if !s.Has(digest(d)) {
		t.Fatal("missing")
	}
	if n, _ := s.Prune(7 * 24 * time.Hour); n != 0 {
		t.Error("an archive that was just asked for was pruned")
	}
}
