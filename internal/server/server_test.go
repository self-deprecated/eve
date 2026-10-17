package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/store"
)

type memSources struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memSources) Has(sha string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[sha]
	return ok
}

func (s *memSources) Put(sha string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.m[sha] = b
	s.mu.Unlock()
	return nil
}

type pipe struct{ run func(jc *queue.JobCtx) error }

func (p pipe) Run(jc *queue.JobCtx) error   { return p.run(jc) }
func (p pipe) Watch(jc *queue.JobCtx) error { return nil }

type fixture struct {
	srv   *httptest.Server
	st    *store.Store
	q     *queue.Queue
	tok   string
	owner string
	sha   string
	dir   string
}

func newFixture(t *testing.T, run func(jc *queue.JobCtx) error) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "eve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q := queue.New(st, dir, map[api.Verb]queue.Pipeline{api.VerbTestflight: pipe{run}}, nil, nil)
	cfg := &config.Server{DataDir: dir, Team: "T", Apps: map[string]config.App{
		"driver":     {BundleID: "ai.selfdeprecated.driver"},
		"screengate": {BundleID: "ai.selfdeprecated.screengate", AllowExec: true},
	}, Notify: config.Notify{AllowHosts: []string{"*.ts.net"}}}
	srcs := &memSources{m: map[string][]byte{}}
	sha := strings.Repeat("a", 64)
	srcs.m[sha] = []byte("x")
	s := &Server{
		Config: cfg, Store: st, Queue: q, Sources: srcs, Version: "test",
		AllowURL: func(raw string, hosts []string) error {
			if strings.Contains(raw, ".ts.net/") {
				return nil
			}
			return errors.New("host not allowed")
		},
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	tok, _ := st.CreateToken(context.Background(), "lattice", false)
	owner, _ := st.CreateToken(context.Background(), "owner", true)
	return &fixture{srv: srv, st: st, q: q, tok: tok, owner: owner, sha: sha, dir: dir}
}

func (f *fixture) do(t *testing.T, method, path, tok string, body any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, r)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestAuthRequired(t *testing.T) {
	f := newFixture(t, nil)
	if resp, _ := f.do(t, "GET", "/v1/version", "", nil); resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
	if resp, _ := f.do(t, "GET", "/v1/version", "eve_wrong", nil); resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
	if resp, _ := f.do(t, "GET", "/v1/version", f.tok, nil); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}

func TestCreateJobValidation(t *testing.T) {
	f := newFixture(t, nil)
	cases := []struct {
		name string
		tok  string
		req  api.JobRequest
		code string
	}{
		{"unknown app", f.tok, api.JobRequest{App: "nope", Verb: "testflight", Source: api.JobSource{SHA256: f.sha}}, "unknown_app"},
		{"bad verb", f.tok, api.JobRequest{App: "driver", Verb: "ship", Source: api.JobSource{SHA256: f.sha}}, "bad_verb"},
		{"exec not allowed", f.tok, api.JobRequest{App: "driver", Verb: "exec", Source: api.JobSource{SHA256: f.sha}}, "exec_not_allowed"},
		{"verb not implemented", f.tok, api.JobRequest{App: "driver", Verb: "device", Source: api.JobSource{SHA256: f.sha}}, "unsupported_verb"},
		{"exec allowed, not implemented", f.tok, api.JobRequest{App: "screengate", Verb: "exec", Source: api.JobSource{SHA256: f.sha}}, "unsupported_verb"},
		{"no source", f.tok, api.JobRequest{App: "driver", Verb: "testflight"}, "no_source"},
		{"unknown source", f.tok, api.JobRequest{App: "driver", Verb: "testflight", Source: api.JobSource{SHA256: strings.Repeat("b", 64)}}, "unknown_source"},
		{"path needs owner", f.tok, api.JobRequest{App: "driver", Verb: "testflight", Source: api.JobSource{Path: "/Users/jmo/Developer/driver"}}, "path_needs_owner"},
		{"url not allowed", f.tok, api.JobRequest{App: "driver", Verb: "testflight", Source: api.JobSource{SHA256: f.sha}, NotifyURLs: []string{"https://evil.example/x"}}, "notify_url_not_allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := f.do(t, "POST", "/v1/jobs", c.tok, c.req)
			var e api.ErrorBody
			_ = json.Unmarshal(body, &e)
			if resp.StatusCode < 400 || e.Code != c.code {
				t.Fatalf("status %d body %s, want code %s", resp.StatusCode, body, c.code)
			}
		})
	}
}

func TestAppsListVerbs(t *testing.T) {
	f := newFixture(t, nil)
	_, body := f.do(t, "GET", "/v1/apps", f.tok, nil)
	var apps []api.App
	if err := json.Unmarshal(body, &apps); err != nil || len(apps) != 2 {
		t.Fatalf("%v %s", err, body)
	}
	// Only testflight has a pipeline here; exec needs one as well as allowExec.
	for _, a := range apps {
		if len(a.Verbs) != 1 || a.Verbs[0] != api.VerbTestflight {
			t.Fatalf("%s verbs %v", a.Name, a.Verbs)
		}
	}
}

func TestCreateJobIdempotentAndRecordsToken(t *testing.T) {
	f := newFixture(t, nil)
	req := api.JobRequest{
		App: "driver", Verb: "testflight", Source: api.JobSource{SHA256: f.sha}, IdempotencyKey: "k1",
		NotifyURLs: []string{"https://quartz.tail6e682f.ts.net/hook"},
	}
	resp, body := f.do(t, "POST", "/v1/jobs", f.tok, req)
	if resp.StatusCode != 201 {
		t.Fatal(resp.StatusCode, string(body))
	}
	var a api.JobCreated
	_ = json.Unmarshal(body, &a)
	resp, body = f.do(t, "POST", "/v1/jobs", f.tok, req)
	var b api.JobCreated
	_ = json.Unmarshal(body, &b)
	if resp.StatusCode != 200 || !b.Existing || b.ID != a.ID {
		t.Fatalf("retry with the same key must return the job: %d %s", resp.StatusCode, body)
	}
	j, _ := f.st.Get(context.Background(), a.ID)
	if j.TokenName != "lattice" || len(j.Request.NotifyURLs) != 1 {
		t.Fatalf("%+v", j)
	}
	// Owner may build a path in place.
	resp, _ = f.do(t, "POST", "/v1/jobs", f.owner, api.JobRequest{App: "driver", Verb: "testflight", Source: api.JobSource{Path: "/Users/jmo/Developer/driver"}})
	if resp.StatusCode != 201 {
		t.Fatal(resp.StatusCode)
	}
}

func TestSourceUpload(t *testing.T) {
	f := newFixture(t, nil)
	sha := strings.Repeat("c", 64)
	if resp, _ := f.do(t, "HEAD", "/v1/sources/"+sha, f.tok, nil); resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
	req, _ := http.NewRequest("PUT", f.srv.URL+"/v1/sources/"+sha, strings.NewReader("tarball"))
	req.Header.Set("Authorization", "Bearer "+f.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatal(err, resp.StatusCode)
	}
	if resp, _ := f.do(t, "HEAD", "/v1/sources/"+sha, f.tok, nil); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if resp, _ := f.do(t, "PUT", "/v1/sources/NOTHEX", f.tok, nil); resp.StatusCode != 400 {
		t.Fatal(resp.StatusCode)
	}
}

func TestEventsStreamEndsOnTerminal(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, func(jc *queue.JobCtx) error {
		_ = jc.Enter(api.StateArchiving, "archiving")
		<-release
		return jc.Succeed("done", nil)
	})
	_, body := f.do(t, "POST", "/v1/jobs", f.tok, api.JobRequest{App: "driver", Verb: "testflight", Source: api.JobSource{SHA256: f.sha}})
	var c api.JobCreated
	_ = json.Unmarshal(body, &c)
	j, _ := f.st.Get(context.Background(), c.ID)
	go f.q.Exec(context.Background(), j)

	req, _ := http.NewRequest("GET", f.srv.URL+"/v1/jobs/"+c.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	var states []string
	sc := bufio.NewScanner(resp.Body)
	deadline := time.After(5 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
loop:
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				break loop
			}
			if d, ok := strings.CutPrefix(l, "data: "); ok {
				var ev api.Event
				_ = json.Unmarshal([]byte(d), &ev)
				states = append(states, string(ev.State))
			}
		case <-deadline:
			t.Fatal("stream did not end")
		}
	}
	got := strings.Join(states, ",")
	if !strings.HasPrefix(got, "queued") || !strings.HasSuffix(got, "succeeded") {
		t.Fatalf("events %s", got)
	}
}

func TestConfirmEndpoint(t *testing.T) {
	f := newFixture(t, nil)
	_, body := f.do(t, "POST", "/v1/jobs", f.tok, api.JobRequest{App: "driver", Verb: "testflight", Source: api.JobSource{SHA256: f.sha}})
	var c api.JobCreated
	_ = json.Unmarshal(body, &c)
	if resp, _ := f.do(t, "POST", "/v1/jobs/"+c.ID+"/confirm", f.tok, map[string]string{"reason": "x"}); resp.StatusCode != 409 {
		t.Fatal("confirm of a queued job must conflict", resp.StatusCode)
	}
	_, _, _ = f.st.Transition(context.Background(), c.ID, store.Transition{To: api.StateAwaitingConfirmation})
	if resp, _ := f.do(t, "POST", "/v1/jobs/"+c.ID+"/confirm", f.tok, map[string]string{}); resp.StatusCode != 409 {
		t.Fatal("confirm without reason must be refused", resp.StatusCode)
	}
	resp, body := f.do(t, "POST", "/v1/jobs/"+c.ID+"/confirm", f.tok, map[string]string{"reason": "added videos"})
	var r api.Result
	_ = json.Unmarshal(body, &r)
	if resp.StatusCode != 200 || r.Outcome != api.StateQueued {
		t.Fatal(resp.StatusCode, string(body))
	}
}

func TestLogTailAndTraversal(t *testing.T) {
	f := newFixture(t, nil)
	if resp, _ := f.do(t, "GET", "/v1/jobs/nope/logs/archiving", f.tok, nil); resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
	_, body := f.do(t, "POST", "/v1/jobs", f.tok, api.JobRequest{App: "driver", Verb: "testflight", Source: api.JobSource{SHA256: f.sha}})
	var c api.JobCreated
	_ = json.Unmarshal(body, &c)
	if resp, _ := f.do(t, "GET", "/v1/jobs/"+c.ID+"/logs/..%2F..%2Feve.db", f.tok, nil); resp.StatusCode == 200 {
		t.Fatal("traversal served a file")
	}
	if got := string(tail([]byte("a\nb\nc\nd\n"), 2)); got != "c\nd\n" {
		t.Fatal(got)
	}
}

func TestDirectoryArtifactIsATarOfItsFiles(t *testing.T) {
	f := newFixture(t, nil)
	_, body := f.do(t, "POST", "/v1/jobs", f.tok, api.JobRequest{App: "driver", Verb: "testflight", Source: api.JobSource{SHA256: f.sha}})
	var c api.JobCreated
	_ = json.Unmarshal(body, &c)
	dir := filepath.Join(f.dir, "jobs", c.ID, "artifacts")
	_ = os.MkdirAll(filepath.Join(dir, "current", "dark"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "current", "01-home.png"), []byte("png"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "current", "dark", "01-home.png"), []byte("dark"), 0o644)
	_ = os.Symlink("/etc/passwd", filepath.Join(dir, "current", "passwd"))
	_ = os.Symlink("/etc", filepath.Join(dir, "etc"))

	resp, b := f.do(t, "GET", "/v1/jobs/"+c.ID+"/artifacts/current", f.tok, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var names []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, " ") != "01-home.png dark/ dark/01-home.png" {
		t.Fatalf("tar entries %v: regular files and directories only", names)
	}
	if resp, _ := f.do(t, "GET", "/v1/jobs/"+c.ID+"/artifacts/etc", f.tok, nil); resp.StatusCode != 404 {
		t.Fatalf("a symlinked artifact was served: %d", resp.StatusCode)
	}
}
