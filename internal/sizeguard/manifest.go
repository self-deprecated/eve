package sizeguard

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Manifest lists every file in an .app bundle with its size (PLAN.md §6.7.2). It is stored per
// job as bundle-manifest.json.zst, and the last accepted TestFlight manifest is the app's baseline.
type Manifest struct {
	Name    string  `json:"name,omitempty"`  // bundle directory name, e.g. "Driver.app"
	Build   string  `json:"build,omitempty"` // identifies the build for reports; set by the caller
	Entries []Entry `json:"entries"`         // sorted by Path
	Total   int64   `json:"total"`           // sum of Size
	Files   int     `json:"files"`           // len(Entries)
}

// Entry is one file or symlink in a bundle.
type Entry struct {
	Path string `json:"path"` // slash-separated, relative to the bundle root
	Size int64  `json:"size"` // 0 for symlinks
	Link string `json:"link,omitempty"`
}

// BuildManifest walks the bundle at appDir. Directories are implied by their files; symlinks are
// recorded with their target and never followed.
func BuildManifest(appDir string) (Manifest, error) {
	m := Manifest{Name: filepath.Base(appDir)}
	err := filepath.WalkDir(appDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(appDir, p)
		if err != nil {
			return err
		}
		e := Entry{Path: filepath.ToSlash(rel)}
		switch t := d.Type(); {
		case t&fs.ModeSymlink != 0:
			if e.Link, err = os.Readlink(p); err != nil {
				return err
			}
		case t.IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			e.Size = fi.Size()
		default:
			return nil // sockets and the like have no place in a bundle and no size
		}
		m.Entries = append(m.Entries, e)
		return nil
	})
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest %s: %w", appDir, err)
	}
	m.normalize()
	return m, nil
}

// normalize sorts the entries and recomputes the totals.
func (m *Manifest) normalize() {
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	m.Total, m.Files = 0, len(m.Entries)
	for _, e := range m.Entries {
		m.Total += e.Size
	}
}

func (m Manifest) index() map[string]Entry {
	idx := make(map[string]Entry, len(m.Entries))
	for _, e := range m.Entries {
		idx[e.Path] = e
	}
	return idx
}

// label is the bundle name used as a path prefix in reports.
func (m Manifest) label() string {
	if m.Name == "" {
		return "App.app"
	}
	return m.Name
}

// WriteManifest writes m to w as zstd-compressed JSON.
func WriteManifest(w io.Writer, m Manifest) error {
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(zw).Encode(m); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

// ReadManifest reads a manifest written by WriteManifest.
func ReadManifest(r io.Reader) (Manifest, error) {
	zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return Manifest{}, err
	}
	defer zr.Close()
	var m Manifest
	if err := json.NewDecoder(zr).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	m.normalize()
	return m, nil
}

// SaveManifest writes m to path atomically.
func SaveManifest(path string, m Manifest) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := WriteManifest(tmp, m); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadManifest reads the manifest at path.
func LoadManifest(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = f.Close() }()
	return ReadManifest(f)
}

// topEntry returns the bundle-root entry that p belongs to: "docs/" for docs/a.md, "Info.plist"
// for Info.plist.
func topEntry(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i+1]
	}
	return p
}
