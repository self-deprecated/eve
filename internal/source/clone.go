package source

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Clone copies the directory tree src to dst, which must not exist. On APFS it makes a
// copy-on-write clone, so a job workspace costs almost nothing however large the source is; on
// other filesystems or platforms it falls back to copying regular files, directories and
// symlinks (symlinks are recreated, never followed).
func Clone(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("clone: %s already exists", dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("clone: %s is not a directory", src)
	}
	if err := cloneTree(src, dst); err == nil {
		return nil
	}
	// A failed clone may leave nothing or a partial tree; start the copy from scratch.
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return copyTree(src, dst)
}

// copyTree copies src to dst entry by entry, preserving permission bits.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.Mkdir(target, fi.Mode().Perm()|0o700)
		case fi.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case fi.Mode().IsRegular():
			return copyFile(p, target, fi.Mode().Perm())
		}
		return fmt.Errorf("clone: %s: unsupported file type %s", p, fi.Mode().Type())
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
