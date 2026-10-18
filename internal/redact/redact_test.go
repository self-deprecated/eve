package redact

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"
)

const (
	home   = "/Users/jmo"
	apiKey = "sk-proj-Abc123+/xyz=SECRET"
)

func newTest() *Redactor {
	return New(Options{Home: home, Secrets: []string{apiKey, "short", "hunter2pass"}})
}

func TestRedact(t *testing.T) {
	r := newTest()
	b64 := base64.StdEncoding.EncodeToString([]byte(apiKey))
	inBlob := base64.StdEncoding.EncodeToString([]byte(`{"user":"x","key":"` + apiKey + `","n":1}`))
	cases := []struct{ name, in, want string }{
		{"plain secret", "key=" + apiKey + " end", "key=[secret] end"},
		{"short secret ignored", "a short word", "a short word"},
		{"query escaped", "u?k=" + url.QueryEscape(apiKey), "u?k=[secret]"},
		{"path escaped", "/x/" + url.PathEscape(apiKey) + "/y", "/x/[secret]/y"},
		{"base64", "Basic " + b64, "Basic [secret]"},
		{"two secrets", "hunter2pass and " + apiKey, "[secret] and [secret]"},
		{"home path", home + "/Developer/driver/ios/build/device.log", "~/Developer/driver/ios/build/device.log"},
		{"home alone", "cd " + home, "cd ~"},
		{"other user", "/Users/jmoore/x and /Users/jmo.bak", "/Users/jmoore/x and /Users/jmo.bak"},
		{"home twice", home + "/a:" + home + "/b", "~/a:~/b"},
		{"email", "Apple ID: jordan.m+dev@example.co.uk signed in", "Apple ID: [email] signed in"},
		{"asset not email", "AppIcon60x60@2x.png logo@Large.png @rpath/Foo.framework", "AppIcon60x60@2x.png logo@Large.png @rpath/Foo.framework"},
		{"macro not email", "macro expansion @Observable:1:45: error: x", "macro expansion @Observable:1:45: error: x"},
		{"udid", "-destination id=00008150-0011AABB22CC33DD done", "-destination id=[udid] done"},
		{"udid lowercase", "id:00008112-000a14a80e08c01e,", "id:[udid],"},
		{"udid 40 hex", "UDID 2b6f0cc904d137be2e1730235f5664094b831186 ok", "UDID [udid] ok"},
		{"udid 40 hex labelled", "{ platform:iOS, id:2b6f0cc904d137be2e1730235f5664094b831186, name:Old }", "{ platform:iOS, id:[udid], name:Old }"},
		{"udid 40 hex identifier", "DeviceIdentifier = 2B6F0CC904D137BE2E1730235F5664094B831186", "DeviceIdentifier = [udid]"},
		{"git hash", "commit a849359be7aaf4986daf5d3019bfcab248bde8e8 (main)", "commit a849359be7aaf4986daf5d3019bfcab248bde8e8 (main)"},
		{"certificate sha-1", "SHA-1: 2B6F0CC904D137BE2E1730235F5664094B831186", "SHA-1: 2B6F0CC904D137BE2E1730235F5664094B831186"},
		{"not a udid", "00008150-0011AABB22CC33DDX and 0000815-0011AABB22CC33DD and deadbeef", "00008150-0011AABB22CC33DDX and 0000815-0011AABB22CC33DD and deadbeef"},
		{"39 hex", strings.Repeat("a", 39), strings.Repeat("a", 39)},
		{"jwt", "token eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJ4In0.c2lnbmF0dXJlX2hlcmU done", "token [token] done"},
		{"bearer", "Authorization: Bearer abcdefghijklmnop0123456789", "Authorization: Bearer [token]"},
		{"bearer prose", "error: Authentication failed: Make sure a bearer token was provided", "error: Authentication failed: Make sure a bearer token was provided"},
		{"pem one line", `{"key":"-----BEGIN PRIVATE KEY-----\nMIGTAgEA\n-----END PRIVATE KEY-----"}`, `{"key":"[private key]"}`},
		{"secret in base64 blob", "payload " + inBlob, ""},
		{"unicode", "Jordan’s iPhone ✔ " + home + "/ä", "Jordan’s iPhone ✔ ~/ä"},
		{"invalid utf8", "\xff\xfe " + apiKey, "\xff\xfe [secret]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := r.String(c.in)
			if c.name == "secret in base64 blob" {
				// The blob's alignment is arbitrary; the secret's own bytes must not survive.
				if got == c.in || !strings.Contains(got, SecretMark) {
					t.Fatalf("secret inside base64 blob not redacted: %q", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestBase64Alignments(t *testing.T) {
	r := New(Options{Secrets: []string{apiKey}})
	for pre := range 3 {
		for post := range 3 {
			raw := strings.Repeat("p", pre) + apiKey + strings.Repeat("q", post)
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding} {
				in := enc.EncodeToString([]byte(raw))
				if got := r.String(in); !strings.Contains(got, SecretMark) {
					t.Errorf("pre=%d post=%d: %q not redacted", pre, post, in)
				}
			}
		}
	}
}

func TestPEMBlock(t *testing.T) {
	r := newTest()
	in := strings.Join([]string{
		"before",
		"cat AuthKey.p8: -----BEGIN PRIVATE KEY-----",
		"MIGTAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBHkwdwIBAQQg",
		"Proc-Type: 4,ENCRYPTED",
		"",
		"oUQDQgAE+abc/def==",
		"-----END PRIVATE KEY----- trailing",
		"after",
		"-----BEGIN RSA PRIVATE KEY-----",
		"MIIEowIBAAKCAQEA",
		"note: Building targets", // a truncated key ends at the first non-key line
		"** ARCHIVE SUCCEEDED **",
	}, "\n")
	want := strings.Join([]string{
		"before",
		"cat AuthKey.p8: [private key]",
		" trailing",
		"after",
		"[private key]",
		"note: Building targets",
		"** ARCHIVE SUCCEEDED **",
	}, "\n")
	if got := r.String(in); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// Streaming one byte at a time must agree.
	var out bytes.Buffer
	w := r.Writer(&out)
	for i := range len(in) {
		_, _ = w.Write([]byte{in[i]})
	}
	_ = w.Close()
	if out.String() != want {
		t.Fatalf("writer got\n%s\nwant\n%s", out.String(), want)
	}
}

func TestMultilineSecret(t *testing.T) {
	secret := "line-one-value\nline-two-value"
	r := New(Options{Secrets: []string{secret}})
	got := r.String("x " + secret + " y")
	if strings.Contains(got, "line-one") || strings.Contains(got, "line-two") {
		t.Fatalf("multi-line secret leaked: %q", got)
	}
}

// TestWriterMatchesRedact feeds realistic text through the Writer at random split points and
// requires the same bytes as Redact, which proves secrets spanning writes are caught.
func TestWriterMatchesRedact(t *testing.T) {
	r := newTest()
	in := []byte(sampleLog(200))
	want := r.Redact(in)
	rng := rand.New(rand.NewPCG(1, 2))
	for round := range 50 {
		var out bytes.Buffer
		w := r.Writer(&out)
		for p := in; len(p) > 0; {
			n := min(len(p), 1+rng.IntN(40))
			if round == 0 {
				n = 1
			}
			if _, err := w.Write(p[:n]); err != nil {
				t.Fatal(err)
			}
			p = p[n:]
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), want) {
			t.Fatalf("round %d: writer output differs from Redact", round)
		}
	}
	if bytes.Contains(want, []byte(apiKey)) || bytes.Contains(want, []byte(home+"/")) {
		t.Fatal("sample still contains a secret or home path")
	}
}

func TestWriterPartialLineAndClose(t *testing.T) {
	r := newTest()
	var out bytes.Buffer
	w := r.Writer(&out)
	_, _ = w.Write([]byte("done " + apiKey[:5]))
	if out.Len() != 0 {
		t.Fatalf("partial line passed on early: %q", out.String())
	}
	_, _ = w.Write([]byte(apiKey[5:]))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "done [secret]" {
		t.Fatalf("got %q", out.String())
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriterError(t *testing.T) {
	w := newTest().Writer(failWriter{})
	if _, err := w.Write([]byte("line\n")); err == nil {
		t.Fatal("want the underlying error")
	}
	if _, err := w.Write([]byte("more\n")); err == nil {
		t.Fatal("error must be sticky")
	}
}

func TestWriterLongLine(t *testing.T) {
	r := newTest()
	// A 3 MiB line without newlines, with secrets placed across every flush boundary.
	var b strings.Builder
	for b.Len() < 3*maxLine {
		b.WriteString(strings.Repeat("x", 997))
		b.WriteString(" " + apiKey + " ")
	}
	in := b.String()
	var out bytes.Buffer
	w := r.Writer(&out)
	for p := in; len(p) > 0; {
		n := min(len(p), 64<<10)
		_, _ = w.Write([]byte(p[:n]))
		p = p[n:]
	}
	_ = w.Close()
	if strings.Contains(out.String(), "sk-proj") {
		t.Fatal("secret leaked across a long-line flush")
	}
	if want := r.String(in); out.String() != want {
		t.Fatal("long-line output differs from Redact")
	}
}

func sampleLog(n int) string {
	var b strings.Builder
	for i := range n {
		switch i % 7 {
		case 0:
			fmt.Fprintf(&b, "CompileSwift normal arm64 %s/Developer/driver/ios/Driver/File%d.swift\n", home, i)
		case 1:
			fmt.Fprintf(&b, "export ASC_KEY=%s\n", apiKey)
		case 2:
			b.WriteString("2026-10-14 13:12:57.566 xcodebuild[29780:32557969] Progress 0 %: Uploading to App Store Connect…\n")
		case 3:
			b.WriteString("    -destination id=00008150-0011AABB22CC33DD -allowProvisioningUpdates\n")
		case 4:
			b.WriteString("Signed in as jordan@example.com with hunter2pass\n")
		case 5:
			b.WriteString("Jordan’s iPhone ✔ Test failedReconnectEndsAsLost() passed after 0.001 seconds.\n")
		default:
			b.WriteString("note: Building targets in dependency order\n")
		}
	}
	return b.String()
}

// benchLog is about 5 MiB of mostly ordinary xcodebuild output.
func benchLog() []byte {
	line := "CompileSwift normal arm64 /Users/jmo/Developer/driver/ios/Driver/Views/ContentView.swift (in target 'Driver' from project 'Driver')\n"
	var b bytes.Buffer
	for b.Len() < 5<<20 {
		b.WriteString(line)
		b.WriteString("    builtin-swiftStdLibTool --copy --verbose --scan-executable /Users/jmo/x/Driver.app/Driver --platform iphoneos\n")
		b.WriteString("2026-10-14 13:12:57.566 xcodebuild[29780:32557969] Progress 0 %: Uploading to App Store Connect…\n")
	}
	b.WriteString("export ASC_KEY=" + apiKey + "\n")
	return b.Bytes()
}

func BenchmarkRedact(b *testing.B) {
	r := newTest()
	in := benchLog()
	b.SetBytes(int64(len(in)))
	for b.Loop() {
		_ = r.Redact(in)
	}
}

func BenchmarkWriter(b *testing.B) {
	r := newTest()
	in := benchLog()
	b.SetBytes(int64(len(in)))
	for b.Loop() {
		var out bytes.Buffer
		out.Grow(len(in))
		w := r.Writer(&out)
		for p := in; len(p) > 0; {
			n := min(len(p), 32<<10)
			_, _ = w.Write(p[:n])
			p = p[n:]
		}
		_ = w.Close()
	}
}
