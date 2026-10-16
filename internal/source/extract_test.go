package source

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// entry is one hand-crafted tar entry.
type entry struct {
	name string
	typ  byte
	body string
	size int64 // overrides len(body) in the header when set; the body is zeros of that size
	link string
	mode int64
}

func reg(name, body string) entry       { return entry{name: name, typ: tar.TypeReg, body: body} }
func dir(name string) entry             { return entry{name: name, typ: tar.TypeDir} }
func symlink(name, target string) entry { return entry{name: name, typ: tar.TypeSymlink, link: target} }

// craft builds a zstd-compressed tar from entries without any of Pack's checks.
func craft(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: e.mode, Format: tar.FormatPAX}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		var body io.Reader = bytes.NewReader([]byte(e.body))
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
			if e.size > 0 {
				hdr.Size = e.size
				body = io.LimitReader(zeros{}, e.size)
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := io.Copy(tw, body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestExtractRefuses(t *testing.T) {
	small := Limits{MaxBytes: 1000, MaxEntries: 5, MaxFileBytes: 600}
	tests := []struct {
		name    string
		entries []entry
		lim     Limits
		want    error
		path    string
	}{
		{"absolute path", []entry{reg("/etc/passwd", "x")}, Limits{}, ErrUnsafePath, "/etc/passwd"},
		{"dot-dot", []entry{reg("../escape", "x")}, Limits{}, ErrUnsafePath, "../escape"},
		{"dot-dot in the middle", []entry{reg("a/../../escape", "x")}, Limits{}, ErrUnsafePath, "a/../../escape"},
		{"dot-dot dir", []entry{dir("../up/")}, Limits{}, ErrUnsafePath, "../up/"},
		{"char device", []entry{{name: "dev/null", typ: tar.TypeChar}}, Limits{}, ErrUnsafePath, "dev/null"},
		{"block device", []entry{{name: "disk", typ: tar.TypeBlock}}, Limits{}, ErrUnsafePath, "disk"},
		{"fifo", []entry{{name: "pipe", typ: tar.TypeFifo}}, Limits{}, ErrUnsafePath, "pipe"},
		{"hardlink", []entry{reg("a", "x"), {name: "b", typ: tar.TypeLink, link: "a"}}, Limits{}, ErrUnsafePath, "b"},
		{"absolute symlink", []entry{symlink("etc", "/etc")}, Limits{}, ErrUnsafePath, "etc"},
		{"symlink climbing out", []entry{symlink("a/up", "../../x")}, Limits{}, ErrUnsafePath, "a/up"},
		{"symlink chain out", []entry{symlink("x/a", ".."), symlink("p", "x/a/../y")}, Limits{}, ErrUnsafePath, "p"},
		{"symlink via self-reference loop", []entry{symlink("l1", "l2"), symlink("l2", "l1/x")}, Limits{}, ErrUnsafePath, "l1"},
		{"file beneath symlink", []entry{symlink("s", "d"), dir("d/"), reg("s/f", "x")}, Limits{}, ErrUnsafePath, "s/f"},
		{"symlink beneath symlink", []entry{symlink("s/inner", "."), symlink("s", "d"), dir("d/")}, Limits{}, ErrUnsafePath, "s/inner"},
		{"duplicate file", []entry{reg("a", "1"), reg("a", "2")}, Limits{}, ErrUnsafePath, "a"},
		{"symlink over file", []entry{reg("a", "1"), symlink("a", "b"), reg("b", "")}, Limits{}, ErrUnsafePath, "a"},
		{"p8 key", []entry{reg("ios/AuthKey_X.p8", "k")}, Limits{}, ErrSecret, "ios/AuthKey_X.p8"},
		{"env file", []entry{reg("web/.env.local", "k")}, Limits{}, ErrSecret, "web/.env.local"},
		{"secret dir", []entry{dir("certs.p12/")}, Limits{}, ErrSecret, "certs.p12"},
		{"provisioning profile", []entry{reg("App.mobileprovision", "p")}, Limits{}, ErrSecret, "App.mobileprovision"},
		{"too many entries", []entry{reg("1", ""), reg("2", ""), reg("3", ""), reg("4", ""), reg("5", ""), reg("6", "")}, small, ErrLimit, "6"},
		{"file too large", []entry{{name: "big", typ: tar.TypeReg, size: 601}}, small, ErrLimit, "big"},
		{"file at limit then total over", []entry{{name: "a", typ: tar.TypeReg, size: 600}, {name: "b", typ: tar.TypeReg, size: 401}}, small, ErrLimit, "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "out")
			err := Extract(bytes.NewReader(craft(t, tt.entries...)), dest, tt.lim)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Extract = %v, want %v", err, tt.want)
			}
			var xe *ExtractError
			if !errors.As(err, &xe) || xe.Path != tt.path {
				t.Errorf("error %v: want path %q", err, tt.path)
			}
		})
	}
}

func TestExtractAcceptsWithinLimits(t *testing.T) {
	small := Limits{MaxBytes: 1000, MaxEntries: 5, MaxFileBytes: 600}
	data := craft(t,
		entry{name: "a", typ: tar.TypeReg, size: 600},
		entry{name: "b", typ: tar.TypeReg, size: 400},
		dir("./d/"), symlink("d/l", "../a"), symlink("top", "d/l"))
	dest := filepath.Join(t.TempDir(), "out")
	if err := Extract(bytes.NewReader(data), dest, small); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "top")); err != nil || len(b) != 600 {
		t.Errorf("chained symlink: %d bytes, %v", len(b), err)
	}
}

func TestExtractCompressionBomb(t *testing.T) {
	// 64 MiB of zeros compresses to a few KiB; a 1 MiB cap must stop it without writing it out.
	lim := Limits{MaxBytes: 1 << 20, MaxEntries: 10, MaxFileBytes: 1 << 30}
	data := craft(t, entry{name: "zeros", typ: tar.TypeReg, size: 64 << 20})
	if len(data) > 64<<10 {
		t.Fatalf("bomb fixture is %d bytes compressed", len(data))
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := Extract(bytes.NewReader(data), dest, lim); !errors.Is(err, ErrLimit) {
		t.Fatalf("Extract = %v, want ErrLimit", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "zeros")); !os.IsNotExist(err) {
		t.Error("bomb content was written")
	}
}

func TestExtractStreamCap(t *testing.T) {
	// Header bloat (huge PAX records) counts against the decompressed-stream cap.
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for i := range 50 {
		hdr := &tar.Header{
			Name: "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Typeflag: tar.TypeReg, Mode: 0o644,
			PAXRecords: map[string]string{"EVE.pad": string(bytes.Repeat([]byte("p"), 500_000))},
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	lim := Limits{MaxBytes: 1, MaxEntries: 100, MaxFileBytes: 1}
	err := Extract(bytes.NewReader(buf.Bytes()), filepath.Join(t.TempDir(), "out"), lim)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("Extract = %v, want ErrLimit", err)
	}
}

func TestExtractNeedsEmptyDest(t *testing.T) {
	dest := t.TempDir()
	writeTree(t, dest, map[string]string{"existing": "x"})
	if err := Extract(bytes.NewReader(craft(t, reg("a", "1"))), dest, Limits{}); err == nil {
		t.Error("extracted into a non-empty directory")
	}
}

func TestExtractGarbage(t *testing.T) {
	if err := Extract(bytes.NewReader([]byte("not zstd")), filepath.Join(t.TempDir(), "o"), Limits{}); err == nil {
		t.Error("garbage accepted")
	}
}
