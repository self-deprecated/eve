package asc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testKey = sync.OnceValue(func() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
})

// call is one request the fake App Store Connect received.
type call struct {
	Method string
	Path   string
	Query  string // raw, as sent
	Auth   string
	Body   map[string]any
}

// fake is a scripted App Store Connect behind httptest, with a fake clock whose sleeps
// advance time instead of waiting.
type fake struct {
	t      *testing.T
	c      *Client
	mu     sync.Mutex
	calls  []call
	now    time.Time
	sleeps []time.Duration
}

func newFake(t *testing.T, h http.HandlerFunc) *fake {
	t.Helper()
	f := &fake{t: t, now: time.Unix(1_800_000_000, 0)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")}
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			if err := json.Unmarshal(data, &c.Body); err != nil {
				t.Errorf("request body is not JSON: %s", data)
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	f.c = &Client{
		BaseURL:  srv.URL,
		KeyID:    "ABCDE12345",
		IssuerID: "issuer",
		key:      testKey(),
		HTTP:     srv.Client(),
		Now:      f.clock,
		sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f.mu.Lock()
			f.sleeps = append(f.sleeps, d)
			f.now = f.now.Add(d)
			f.mu.Unlock()
			return nil
		},
	}
	return f
}

func (f *fake) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fake) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// count returns how many requests hit path with method ("" matches any method).
func (f *fake) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Path == path && (method == "" || c.Method == method) {
			n++
		}
	}
	return n
}

// find returns the first request to path with method.
func (f *fake) find(method, path string) (call, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Path == path && (method == "" || c.Method == method) {
			return c, true
		}
	}
	return call{}, false
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	switch b := body.(type) {
	case string:
		_, _ = io.WriteString(w, b)
	default:
		_ = json.NewEncoder(w).Encode(b)
	}
}

func appleError(code, title string) map[string]any {
	return map[string]any{"errors": []map[string]string{{"code": code, "title": title}}}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// build is a builds resource as the API returns it.
func buildJSON(id, number, processing, audience string) map[string]any {
	return map[string]any{
		"type": "builds",
		"id":   id,
		"attributes": map[string]any{
			"version":                 number,
			"processingState":         processing,
			"buildAudienceType":       audience,
			"usesNonExemptEncryption": false,
		},
	}
}

func detailJSON(id, internal string) map[string]any {
	return map[string]any{
		"type":       "buildBetaDetails",
		"id":         id,
		"attributes": map[string]any{"internalBuildState": internal},
	}
}

func uploadJSON(state string, errs ...[2]string) map[string]any {
	items := []map[string]string{}
	for _, e := range errs {
		items = append(items, map[string]string{"code": e[0], "description": e[1]})
	}
	return map[string]any{
		"type":       "buildUploads",
		"id":         "u-" + strings.ToLower(state),
		"attributes": map[string]any{"state": map[string]any{"state": state, "errors": items}},
	}
}

func list(items ...map[string]any) map[string]any {
	if items == nil {
		items = []map[string]any{}
	}
	return map[string]any{"data": items}
}
