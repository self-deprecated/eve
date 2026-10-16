package source

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Limits caps what Extract will write. Zero fields take the defaults.
type Limits struct {
	MaxBytes     int64 // total size of all files; default 2 GiB
	MaxEntries   int   // archive entries of any kind; default 200,000
	MaxFileBytes int64 // any single file; default 1 GiB
}

// DefaultLimits are the caps from PLAN.md §6.1.
var DefaultLimits = Limits{MaxBytes: 2 << 30, MaxEntries: 200_000, MaxFileBytes: 1 << 30}

func (l Limits) withDefaults() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultLimits.MaxBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultLimits.MaxEntries
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultLimits.MaxFileBytes
	}
	return l
}

// Extraction refusals. ExtractError wraps one of these with the offending path.
var (
	ErrUnsafePath = errors.New("unsafe archive entry")
	ErrLimit      = errors.New("archive exceeds limits")
	ErrSecret     = errors.New("archive contains credential files")
)

// ExtractError reports why Extract refused an archive.
type ExtractError struct {
	Err    error // ErrUnsafePath, ErrLimit or ErrSecret
	Path   string
	Detail string
}

func (e *ExtractError) Error() string {
	msg := "extract: " + e.Err.Error()
	if e.Path != "" {
		msg += fmt.Sprintf(" %q", e.Path)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *ExtractError) Unwrap() error { return e.Err }

func refuse(kind error, p, detail string) error {
	return &ExtractError{Err: kind, Path: p, Detail: detail}
}

// maxSymlinkHops bounds symlink resolution, as the kernel's ELOOP does.
const maxSymlinkHops = 40

// Extract unpacks a zstd-compressed tar from r into dest, treating it as hostile (PLAN.md §6.1,
// §14). dest must be absent or an empty directory. Only regular files, directories and symlinks
// are accepted; absolute paths, ".." segments, hardlinks, devices and FIFOs are refused, as are
// symlinks that resolve outside dest and entries beneath a symlink. Files matching SecretPatterns
// are refused. The limits are enforced from the tar headers before any content is written, and
// the decompressed stream itself is capped, so a compression bomb stops early.
//
// On error dest may hold a partial tree; the caller removes it.
func Extract(r io.Reader, dest string, lim Limits) error {
	lim = lim.withDefaults()
	if err := prepareDest(dest); err != nil {
		return err
	}
	zr, err := zstd.NewReader(r,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(64<<20))
	if err != nil {
		return err
	}
	defer zr.Close()
	// Headers and padding cost up to ~1.5 KiB per entry beyond the file content.
	raw := &capReader{r: zr, left: lim.MaxBytes + int64(lim.MaxEntries)*2048 + 1<<20}
	tr := tar.NewReader(raw)

	x := extractor{dest: dest, lim: lim, links: map[string]string{}}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if raw.exceeded {
				return refuse(ErrLimit, "", "decompressed stream too large")
			}
			return fmt.Errorf("extract: %w", err)
		}
		if err := x.entry(tr, hdr); err != nil {
			if raw.exceeded {
				return refuse(ErrLimit, "", "decompressed stream too large")
			}
			return err
		}
	}
	return x.finishLinks()
}

func prepareDest(dest string) error {
	entries, err := os.ReadDir(dest)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(dest, 0o755)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("extract: destination %s is not empty", dest)
	}
	return nil
}

type extractor struct {
	dest    string
	lim     Limits
	entries int
	total   int64
	links   map[string]string // deferred symlinks: rel → target
}

func (x *extractor) entry(tr *tar.Reader, hdr *tar.Header) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}
	x.entries++
	if x.entries > x.lim.MaxEntries {
		return refuse(ErrLimit, hdr.Name, fmt.Sprintf("more than %d entries", x.lim.MaxEntries))
	}
	name := strings.TrimPrefix(hdr.Name, "./")
	if hdr.Typeflag == tar.TypeDir {
		name = strings.TrimSuffix(name, "/")
		if name == "" || name == "." {
			return nil
		}
	}
	if !validRel(name) {
		return refuse(ErrUnsafePath, hdr.Name, "absolute or non-canonical path")
	}
	if IsSecret(name) {
		return refuse(ErrSecret, name, "matches a credential pattern")
	}
	if p := x.symlinkParent(name); p != "" {
		return refuse(ErrUnsafePath, name, "lies beneath symlink "+p)
	}
	target := filepath.Join(x.dest, filepath.FromSlash(name))

	switch hdr.Typeflag {
	case tar.TypeDir:
		return mkdir(target, name)
	case tar.TypeReg: // tar.Reader already maps the legacy TypeRegA to TypeReg or TypeDir
		if hdr.Size > x.lim.MaxFileBytes {
			return refuse(ErrLimit, name, fmt.Sprintf("file is %d bytes, limit %d", hdr.Size, x.lim.MaxFileBytes))
		}
		x.total += hdr.Size
		if x.total > x.lim.MaxBytes {
			return refuse(ErrLimit, name, fmt.Sprintf("total exceeds %d bytes", x.lim.MaxBytes))
		}
		return writeFile(tr, target, name, hdr)
	case tar.TypeSymlink:
		if _, dup := x.links[name]; dup {
			return refuse(ErrUnsafePath, name, "duplicate entry")
		}
		x.links[name] = hdr.Linkname
		return nil
	case tar.TypeLink:
		return refuse(ErrUnsafePath, name, "hardlinks are not accepted")
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return refuse(ErrUnsafePath, name, "device or FIFO")
	default:
		return refuse(ErrUnsafePath, name, fmt.Sprintf("unsupported entry type %q", hdr.Typeflag))
	}
}

// symlinkParent returns the first proper ancestor of rel that is a symlink entry, or "".
func (x *extractor) symlinkParent(rel string) string {
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			if _, ok := x.links[rel[:i]]; ok {
				return rel[:i]
			}
		}
	}
	return ""
}

func mkdir(target, name string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return refuse(ErrUnsafePath, name, err.Error())
	}
	return nil
}

func writeFile(r io.Reader, target, name string, hdr *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return refuse(ErrUnsafePath, name, err.Error())
	}
	mode := os.FileMode(0o644)
	if hdr.Mode&0o111 != 0 {
		mode = 0o755
	}
	// O_EXCL: a duplicate entry must not overwrite, or write through, what is already there.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return refuse(ErrUnsafePath, name, err.Error())
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("extract %s: %w", name, err)
	}
	if n != hdr.Size {
		return fmt.Errorf("extract %s: wrote %d of %d bytes", name, n, hdr.Size)
	}
	return nil
}

// finishLinks creates the deferred symlinks once every file and directory exists, after checking
// that each one resolves inside dest given all the other symlinks in the archive.
func (x *extractor) finishLinks() error {
	names := make([]string, 0, len(x.links))
	for n := range x.links {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if p := x.symlinkParent(name); p != "" {
			return refuse(ErrUnsafePath, name, "lies beneath symlink "+p)
		}
		target := x.links[name]
		if _, ok := x.resolve(splitRel(path.Dir(name)), target, 0); !ok {
			return refuse(ErrUnsafePath, name, "symlink target "+target+" resolves outside the tree")
		}
	}
	for _, name := range names {
		full := filepath.Join(x.dest, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return refuse(ErrUnsafePath, name, err.Error())
		}
		if err := os.Symlink(x.links[name], full); err != nil {
			return refuse(ErrUnsafePath, name, err.Error())
		}
	}
	return nil
}

// resolve follows target from the directory dir (path segments below dest) the way the kernel
// would, expanding symlinks from the archive along the way. ok is false when resolution climbs
// above dest, uses an absolute target, or loops. dest started empty, so the archive's symlinks are
// the only ones that can be met.
func (x *extractor) resolve(dir []string, target string, hops int) ([]string, bool) {
	if hops > maxSymlinkHops || target == "" || path.IsAbs(target) || filepath.IsAbs(target) {
		return nil, false
	}
	cur := append([]string(nil), dir...)
	for seg := range strings.SplitSeq(target, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(cur) == 0 {
				return nil, false
			}
			cur = cur[:len(cur)-1]
			continue
		}
		cur = append(cur, seg)
		if t, ok := x.links[strings.Join(cur, "/")]; ok {
			var resolved []string
			if resolved, ok = x.resolve(cur[:len(cur)-1], t, hops+1); !ok {
				return nil, false
			}
			cur = resolved
		}
	}
	return cur, true
}

func splitRel(dir string) []string {
	if dir == "." || dir == "" {
		return nil
	}
	return strings.Split(dir, "/")
}

// capReader fails once more than left bytes have been read.
type capReader struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		c.exceeded = true
		return 0, ErrLimit
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
