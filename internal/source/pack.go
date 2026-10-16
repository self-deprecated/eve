package source

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/self-deprecated/eve/internal/api"
)

// PackInfo describes a packed source archive.
type PackInfo struct {
	SHA256 string // of the compressed archive: the content address
	Bytes  int64  // compressed size
	Stats  api.SourceStats
}

// epoch is the modification time of every archive entry, so identical trees pack identically.
var epoch = time.Unix(0, 0).UTC()

// Pack writes files (relative to root) to w as a zstd-compressed PAX tar. The archive is
// deterministic: entries are sorted, directories are implied, and owners, times and modes are
// normalized (0644, or 0755 when any execute bit is set), so identical trees give identical
// SHA-256 digests. Symlinks are kept when their target stays inside root; anything else is an
// error, as is a file that changes type while being packed.
func Pack(root string, files []File, w io.Writer) (PackInfo, error) {
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Rel < sorted[j].Rel })

	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(w, h)}
	zw, err := zstd.NewWriter(cw, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return PackInfo{}, err
	}
	tw := tar.NewWriter(zw)

	packed := make([]File, 0, len(sorted))
	for i, f := range sorted {
		if i > 0 && sorted[i-1].Rel == f.Rel {
			continue
		}
		pf, err := packOne(tw, root, f.Rel)
		if err != nil {
			_ = zw.Close()
			return PackInfo{}, err
		}
		packed = append(packed, pf)
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return PackInfo{}, err
	}
	if err := zw.Close(); err != nil {
		return PackInfo{}, err
	}
	return PackInfo{SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: cw.n, Stats: Stats(packed)}, nil
}

func packOne(tw *tar.Writer, root, rel string) (File, error) {
	if !validRel(rel) {
		return File{}, fmt.Errorf("pack: invalid path %q", rel)
	}
	f, ok, err := statFile(root, rel)
	if err != nil {
		return File{}, err
	}
	if !ok {
		return File{}, fmt.Errorf("pack: %s is no longer a regular file or symlink", rel)
	}
	hdr := &tar.Header{
		Name:    rel,
		ModTime: epoch,
		Format:  tar.FormatPAX,
	}
	if f.Mode&fs.ModeSymlink != 0 {
		if !linkInside(rel, f.Link) {
			return File{}, fmt.Errorf("pack: symlink %s -> %s points outside the source tree", rel, f.Link)
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = f.Link
		hdr.Mode = 0o777
		return f, tw.WriteHeader(hdr)
	}
	hdr.Typeflag = tar.TypeReg
	hdr.Size = f.Size
	hdr.Mode = 0o644
	if f.Mode&0o111 != 0 {
		hdr.Mode = 0o755
	}
	src, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return File{}, err
	}
	defer func() { _ = src.Close() }()
	if err := tw.WriteHeader(hdr); err != nil {
		return File{}, err
	}
	if _, err := io.CopyN(tw, src, f.Size); err != nil {
		return File{}, fmt.Errorf("pack %s: %w (did it change while packing?)", rel, err)
	}
	return f, nil
}

// validRel reports whether rel is a clean, relative, slash-separated path inside the root.
func validRel(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) {
		return false
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// linkInside reports whether a symlink at rel with the given target stays inside the root,
// judged lexically: the target must be relative and must not climb above the root.
func linkInside(rel, target string) bool {
	if target == "" || path.IsAbs(target) || filepath.IsAbs(target) {
		return false
	}
	joined := path.Join(path.Dir(rel), target)
	return joined != ".." && !strings.HasPrefix(joined, "../")
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
