package flock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestLockContextWaitCancelsWithoutAcquiringLater(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := LockContext(ctx, b); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait: %v, want deadline exceeded", err)
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	if err := Lock(a, false); err != nil {
		t.Fatalf("cancelled wait acquired a lock later: %v", err)
	}
}
