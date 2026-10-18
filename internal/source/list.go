// Package source handles a job's source tree end to end (docs/design.md §6.1): listing what to upload
// (git or a .gitignore-aware walk, minus .eveignore and the always-excluded list), packing it as a
// deterministic tar.zst, storing uploads by SHA-256 on the server, extracting them as hostile
// input, and cloning the result into a job workspace with APFS copy-on-write.
package source

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
)

// EveIgnoreFile is the repository-root file with extra exclusions for eve only.
const EveIgnoreFile = ".eveignore"

// File is one entry of a source listing.
type File struct {
	Rel  string      // slash-separated path relative to the root
	Size int64       // bytes; 0 for symlinks
	Mode fs.FileMode // from lstat: type bits and permissions
	Link string      // symlink target, as stored
}

// ListResult is what List found.
type ListResult struct {
	// Files are the entries to upload, sorted by Rel.
	Files []File
	// Excluded are entries that git (or the .gitignore walk) would have included but the
	// always-excluded list removed: build output, dependency caches, credentials. They are not
	// uploaded; the size guard uses them to warn about an incomplete .gitignore.
	Excluded []File
	// Git reports whether the listing came from git rather than a directory walk.
	Git bool
}

// List returns the files under root to upload. Inside a git work tree it asks git for tracked
// and untracked-but-not-ignored files; otherwise it walks the tree honouring every .gitignore.
// Either way it then drops .eveignore and extraIgnore matches (silently) and AlwaysExclude and
// SecretPatterns matches (into Excluded). Only regular files and symlinks are listed.
func List(root string, extraIgnore []string) (ListResult, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return ListResult{}, err
	}
	user := &Ignore{}
	if err := user.AddFile("", filepath.Join(root, EveIgnoreFile)); err != nil {
		return ListResult{}, err
	}
	user.Add("", extraIgnore)

	var res ListResult
	if inGitWorkTree(root) {
		if paths, ok := gitFiles(root); ok {
			res, err = listGit(root, paths, user)
			res.Git = true
		} else {
			res, err = listWalk(root, user)
		}
	} else {
		res, err = listWalk(root, user)
	}
	if err != nil {
		return ListResult{}, err
	}
	sortFiles(res.Files)
	sortFiles(res.Excluded)
	return res, nil
}

func sortFiles(fs []File) {
	sort.Slice(fs, func(i, j int) bool { return fs[i].Rel < fs[j].Rel })
}

// listGit stats the paths git reported, skipping those deleted from the work tree and
// directories (submodules).
func listGit(root string, paths []string, user *Ignore) (ListResult, error) {
	var res ListResult
	var exe map[string]bool
	if runtime.GOOS == "windows" {
		exe = gitExecutables(root)
	}
	for _, rel := range paths {
		if user.Ignored(rel, false) {
			continue
		}
		f, ok, err := statFile(root, rel)
		if err != nil {
			return ListResult{}, err
		}
		if !ok {
			continue
		}
		if exe[rel] {
			f.Mode |= 0o111
		}
		if alwaysExclude.Ignored(rel, false) {
			res.Excluded = append(res.Excluded, f)
		} else {
			res.Files = append(res.Files, f)
		}
	}
	return res, nil
}

// listWalk walks root, loading each directory's .gitignore before visiting its entries.
// Directories matched only by the always-excluded list are still walked so their contents can be
// reported in Excluded.
func listWalk(root string, user *Ignore) (ListResult, error) {
	var res ListResult
	gi := &Ignore{}
	excludedDir := "" // the always-excluded directory currently being walked, if any
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		isDir := d.IsDir()
		if rel == "." {
			return gi.AddFile("", filepath.Join(path, ".gitignore"))
		}
		if excludedDir != "" && !strings.HasPrefix(rel, excludedDir+"/") {
			excludedDir = ""
		}
		if isDir && d.Name() == ".git" {
			return fs.SkipDir
		}
		if gi.Match(rel, isDir) || user.Match(rel, isDir) {
			if isDir {
				return fs.SkipDir
			}
			return nil
		}
		if isDir {
			if excludedDir == "" && alwaysExclude.Match(rel, true) {
				excludedDir = rel
			}
			return gi.AddFile(rel, filepath.Join(path, ".gitignore"))
		}
		f, ok, err := statFile(root, rel)
		if err != nil || !ok {
			return err
		}
		if excludedDir != "" || alwaysExclude.Match(rel, false) {
			res.Excluded = append(res.Excluded, f)
		} else {
			res.Files = append(res.Files, f)
		}
		return nil
	})
	return res, err
}

// statFile lstats root/rel. ok is false for paths that vanished, directories and special files.
func statFile(root, rel string) (File, bool, error) {
	path := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, err
	}
	f := File{Rel: rel, Mode: fi.Mode()}
	switch {
	case fi.Mode().IsRegular():
		f.Size = fi.Size()
	case fi.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(path)
		if err != nil {
			return File{}, false, fmt.Errorf("readlink %s: %w", rel, err)
		}
		// Windows reports targets with backslashes; the archive and the escape check use slashes.
		f.Link = filepath.ToSlash(link)
	default:
		return File{}, false, nil
	}
	return f, true, nil
}

// Stats summarizes files for the size guard (docs/design.md §6.7.1). TopDir is keyed by the first path
// segment, so a top-level directory and a top-level file are both one key.
func Stats(files []File) api.SourceStats {
	st := api.SourceStats{TopDir: map[string]int64{}}
	for _, f := range files {
		st.Bytes += f.Size
		st.Files++
		top, _, _ := strings.Cut(f.Rel, "/")
		st.TopDir[top] += f.Size
	}
	return st
}
