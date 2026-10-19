// Package flock takes exclusive advisory locks on files: flock(2) on Unix, LockFileEx on Windows.
package flock

import (
	"context"
	"errors"
	"os"
	"time"
)

// ErrLocked is returned by Lock with wait false when another process holds the lock.
var ErrLocked = errors.New("locked by another process")

// Lock takes an exclusive lock on f, waiting for it when wait is true.
func Lock(f *os.File, wait bool) error { return lock(f, wait) }

// LockContext waits for an exclusive lock, stopping when ctx ends. It uses non-blocking
// attempts so cancellation never leaves a goroutine that can acquire the lock later.
func LockContext(ctx context.Context, f *os.File) error {
	retry := time.NewTicker(50 * time.Millisecond)
	defer retry.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := Lock(f, false); !errors.Is(err, ErrLocked) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-retry.C:
		}
	}
}

// Unlock releases a lock taken with Lock.
func Unlock(f *os.File) error { return unlock(f) }
