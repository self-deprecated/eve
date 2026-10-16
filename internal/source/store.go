package source

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store holds uploaded source archives on the server, addressed by the SHA-256 of their bytes
// (PLAN.md §4.3: sources/<sha256>.tar.zst).
type Store struct {
	Dir string
}

// ErrBadDigest is returned for a malformed digest or an upload whose content does not match it.
var ErrBadDigest = errors.New("source digest mismatch")

const (
	archiveExt = ".tar.zst"
	tempPrefix = ".upload-"
)

// ValidSHA256 reports whether s is a lowercase hex SHA-256 digest.
func ValidSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Path returns where the archive for sha lives.
func (s Store) Path(sha string) string { return filepath.Join(s.Dir, sha+archiveExt) }

// Has reports whether the archive for sha is stored. A hit refreshes its modification time, so
// a source that is still being reused is not pruned.
func (s Store) Has(sha string) bool {
	if !ValidSHA256(sha) {
		return false
	}
	p := s.Path(sha)
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return true
}

// Put stores the archive read from r under sha. The bytes go to a temporary file first and are
// renamed into place only when their SHA-256 matches, so readers never see a partial archive.
func (s Store) Put(sha string, r io.Reader) (err error) {
	if !ValidSHA256(sha) {
		return fmt.Errorf("%w: %q is not a SHA-256 digest", ErrBadDigest, sha)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, tempPrefix+"*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return fmt.Errorf("%w: content hashes to %s, not %s", ErrBadDigest, got, sha)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path(sha))
}

// Open opens the stored archive for sha.
func (s Store) Open(sha string) (*os.File, error) {
	if !ValidSHA256(sha) {
		return nil, fmt.Errorf("%w: %q is not a SHA-256 digest", ErrBadDigest, sha)
	}
	return os.Open(s.Path(sha))
}

// Prune removes archives (and abandoned temporary uploads) not used for longer than olderThan,
// and returns how many files it removed.
func (s Store) Prune(olderThan time.Duration) (int, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	var errs []error
	for _, e := range entries {
		name := e.Name()
		ours := strings.HasPrefix(name, tempPrefix) ||
			(strings.HasSuffix(name, archiveExt) && ValidSHA256(strings.TrimSuffix(name, archiveExt)))
		if !ours || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.Dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
