package flock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSecondLockFailsWithoutWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	if err := Lock(a, false); err != nil {
		t.Fatal(err)
	}
	if err := Lock(b, false); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v, want ErrLocked", err)
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	if err := Lock(b, false); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
}
