// Package flock takes exclusive advisory locks on files: flock(2) on Unix, LockFileEx on Windows.
package flock

import (
	"errors"
	"os"
)

// ErrLocked is returned by Lock with wait false when another process holds the lock.
var ErrLocked = errors.New("locked by another process")

// Lock takes an exclusive lock on f, waiting for it when wait is true.
func Lock(f *os.File, wait bool) error { return lock(f, wait) }

// Unlock releases a lock taken with Lock.
func Unlock(f *os.File) error { return unlock(f) }
