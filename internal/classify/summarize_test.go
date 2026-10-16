package classify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResponsesSummarize(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if a := r.Header.Get("Authorization"); a != "Bearer k-123" {
			t.Errorf("Authorization %q", a)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"status":"completed","output":[
			{"type":"reasoning","summary":[]},
			{"type":"message","role":"assistant","content":[{"type":"output_text",
			 "text":"{\"summary\":\"SPM couldn't resolve Capacitor's binary targets.\",\"hint\":\"Run the package resolution step before building.\",\"confidence\":0.8}"}]}]}`))
	}))
	defer srv.Close()

	s := Responses{
		BaseURL: srv.URL + "/v1/", Model: "gpt-6-luna", APIKeyEnv: "CLIPROXY_API_KEY",
		Getenv: env(map[string]string{"CLIPROXY_API_KEY": "k-123"}),
	}
	summary, hint, conf, err := s.Summarize(context.Background(), []string{"error: There is no XCFramework found"}, api.StateBuilding)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(summary, "SPM couldn't") || !strings.HasPrefix(hint, "Run the") || conf != 0.8 {
		t.Fatalf("%q %q %v", summary, hint, conf)
	}
	if got["model"] != "gpt-6-luna" || got["store"] != false {
		t.Fatalf("request %v", got)
	}
	if in, _ := got["input"].(string); !strings.Contains(in, "Phase: building") || !strings.Contains(in, "XCFramework") {
		t.Fatalf("input %q", in)
	}
}

func TestResponsesErrors(t *testing.T) {
	reply := ""
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	s := Responses{BaseURL: srv.URL, Model: "m", APIKeyEnv: "K", Getenv: env(map[string]string{"K": "v"})}
	cases := []struct {
		name   string
		status int
		reply  string
		want   string
	}{
		{"http error", http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`, "429"},
		{"api error", http.StatusOK, `{"error":{"message":"bad model"}}`, "bad model"},
		{"no text", http.StatusOK, `{"output":[]}`, "no output text"},
		{"not json", http.StatusOK, `{"output_text":"I think it is signing"}`, "requested JSON"},
		{"invalid body", http.StatusOK, `<html>`, "invalid JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, reply = c.status, c.reply
			_, _, _, err := s.Summarize(context.Background(), []string{"x"}, api.StateBuilding)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
		})
	}
	noKey := Responses{BaseURL: srv.URL, APIKeyEnv: "MISSING", Getenv: env(nil)}
	if _, _, _, err := noKey.Summarize(context.Background(), nil, ""); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("missing key: %v", err)
	}
}

type fakeSummarizer struct {
	summary, hint string
	conf          float64
	err           error
	calls         int
	excerpt       []string
}

func (f *fakeSummarizer) Summarize(_ context.Context, ex []string, _ api.State) (string, string, float64, error) {
	f.calls++
	f.excerpt = ex
	return f.summary, f.hint, f.conf, f.err
}

func TestRefine(t *testing.T) {
	unknown := Classify(Input{Phase: api.StateBuilding, Log: []byte("error: There is no XCFramework found\n"), ExitCode: 65})
	known := Classify(Input{Phase: api.StateBuilding, Log: []byte("error: Signing for \"A\" requires a development team.\n")})

	s := &fakeSummarizer{summary: "Binary target missing.", hint: "Resolve packages.", conf: 0.9}
	got := Refine(context.Background(), s, unknown, api.StateBuilding, nil)
	if got.By != "model" || got.Summary != "Binary target missing." || got.Class != ClassUnknown || unknown.By != "rules" {
		t.Fatalf("refined %+v (original %+v)", got, unknown)
	}
	if len(s.excerpt) == 0 || s.excerpt[0] != unknown.Excerpt[0] {
		t.Fatalf("model saw %q", s.excerpt)
	}
	if Refine(context.Background(), s, known, api.StateBuilding, nil) != known || s.calls != 1 {
		t.Fatal("a classified failure must not reach the model")
	}
	if Refine(context.Background(), nil, unknown, api.StateBuilding, nil) != unknown {
		t.Fatal("nil summarizer (the default) must be a no-op")
	}
	for _, f := range []*fakeSummarizer{
		{summary: "guess", conf: 0.1},
		{summary: "", conf: 0.9},
		{err: errors.New("down")},
	} {
		if got := Refine(context.Background(), f, unknown, api.StateBuilding, []string{"own"}); got != unknown {
			t.Fatalf("%+v replaced the rules result", f)
		}
		if f.excerpt[0] != "own" {
			t.Fatalf("caller excerpt not passed: %q", f.excerpt)
		}
	}
}
