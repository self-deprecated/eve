package t3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAuth is an MCP OAuth authorization server shaped like T3's (spike/m0-wakeup/RESULT.md).
type fakeAuth struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	registrations int
	redirects     map[string]string // client_id → registered redirect URI
	challenge     string            // from the last authorize request
	redirectIss   string            // iss to put in the redirect; "" means the real issuer
	expiresIn     int
}

func newFakeAuth(t *testing.T) *fakeAuth {
	f := &fakeAuth{t: t, redirects: map[string]string{}, expiresIn: 30 * 24 * 3600}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              f.srv.URL + "/mcp",
			"authorization_servers": []string{f.srv.URL},
			"scopes_supported":      []string{"orchestration:read", "orchestration:operate"},
		})
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                         f.srv.URL,
			"authorization_endpoint":                         f.srv.URL + "/oauth/mcp/authorize",
			"token_endpoint":                                 f.srv.URL + "/oauth/mcp/token",
			"registration_endpoint":                          f.srv.URL + "/oauth/mcp/register",
			"code_challenge_methods_supported":               []string{"S256"},
			"token_endpoint_auth_methods_supported":          []string{"none"},
			"authorization_response_iss_parameter_supported": true,
		})
	})
	mux.HandleFunc("POST /oauth/mcp/register", f.register)
	mux.HandleFunc("GET /oauth/mcp/authorize", f.authorize)
	mux.HandleFunc("POST /oauth/mcp/token", f.token)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAuth) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.ClientName != "eve" || req.AuthMethod != "none" || len(req.RedirectURIs) != 1 ||
		!strings.HasPrefix(req.RedirectURIs[0], "http://127.0.0.1:") || !strings.HasSuffix(req.RedirectURIs[0], "/callback") {
		f.t.Errorf("bad registration %+v", req)
		http.Error(w, `{"error":"invalid_client_metadata"}`, 400)
		return
	}
	f.mu.Lock()
	f.registrations++
	id := "client-" + string(rune('0'+f.registrations))
	f.redirects[id] = req.RedirectURIs[0]
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"client_id": id, "redirect_uris": req.RedirectURIs})
}

func (f *fakeAuth) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	redirect := f.redirects[q.Get("client_id")]
	f.challenge = q.Get("code_challenge")
	iss := f.redirectIss
	f.mu.Unlock()
	switch {
	case redirect == "" || q.Get("redirect_uri") != redirect:
		http.Error(w, "unknown client or redirect", 400)
		return
	case q.Get("response_type") != "code", q.Get("code_challenge_method") != "S256",
		q.Get("scope") != Scope, q.Get("resource") != f.srv.URL+"/mcp", q.Get("state") == "":
		http.Error(w, "bad authorize request: "+r.URL.RawQuery, 400)
		return
	}
	if iss == "" {
		iss = f.srv.URL
	}
	cb, _ := url.Parse(redirect)
	cb.RawQuery = url.Values{"code": {"the-code"}, "state": {q.Get("state")}, "iss": {iss}}.Encode()
	http.Redirect(w, r, cb.String(), http.StatusFound)
}

func (f *fakeAuth) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case r.PostForm.Get("grant_type") != "authorization_code", r.PostForm.Get("code") != "the-code":
		http.Error(w, `{"error":"invalid_grant"}`, 400)
	case base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge:
		http.Error(w, `{"error":"invalid_grant","error_description":"PKCE verifier mismatch"}`, 400)
	case f.redirects[r.PostForm.Get("client_id")] != r.PostForm.Get("redirect_uri"):
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
	case r.PostForm.Get("resource") != f.srv.URL+"/mcp":
		http.Error(w, `{"error":"invalid_target"}`, 400)
	default:
		writeJSON(w, map[string]any{
			"access_token": "tok-123", "token_type": "Bearer", "expires_in": f.expiresIn,
			"scope": Scope,
		})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// browser follows the authorization URL like a user approving it, including the redirect to
// Eve's loopback callback.
func browser(t *testing.T) func(string) error {
	return func(u string) error {
		resp, err := http.Get(u)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
}

func newStore(t *testing.T) *TokenStore {
	return &TokenStore{Path: filepath.Join(t.TempDir(), "eve", "t3-token")}
}

func loginCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLoginFullFlow(t *testing.T) {
	f := newFakeAuth(t)
	store := newStore(t)
	if err := Login(loginCtx(t), f.srv.URL, browser(t), store); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode %04o, want 0600", perm)
	}
	tok, err := store.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "tok-123" || tok.Scope != Scope || tok.Resource != f.srv.URL+"/mcp" || tok.Issuer != f.srv.URL {
		t.Errorf("stored token %+v", tok)
	}
	if d := time.Until(tok.ExpiresAt); d < 29*24*time.Hour || d > 30*24*time.Hour {
		t.Errorf("expires_at %v not ~30 days out", tok.ExpiresAt)
	}
	reg, err := store.Registration()
	if err != nil || reg == nil || reg.ClientID != "client-1" {
		t.Fatalf("registration %+v, %v", reg, err)
	}
	if st := store.Status(); st.State != TokenValid {
		t.Errorf("status %+v, want valid", st)
	}

	// A second login reuses the stored registration and its port.
	if err := Login(loginCtx(t), f.srv.URL, browser(t), store); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registrations != 1 {
		t.Errorf("registrations = %d, want 1 (reused)", f.registrations)
	}
}

func TestLoginRejectsWrongIssuer(t *testing.T) {
	f := newFakeAuth(t)
	f.redirectIss = "http://evil.example"
	store := newStore(t)
	err := Login(loginCtx(t), f.srv.URL, browser(t), store)
	if err == nil || !strings.Contains(err.Error(), "iss") {
		t.Fatalf("err = %v, want iss mismatch", err)
	}
	if _, err := store.Token(); err != ErrNoToken {
		t.Errorf("token stored despite failure: %v", err)
	}
}

func TestLoginIgnoresForgedState(t *testing.T) {
	f := newFakeAuth(t)
	store := newStore(t)
	open := func(u string) error {
		// A forged callback first: rejected without ending the flow.
		au, _ := url.Parse(u)
		cb := au.Query().Get("redirect_uri") + "?code=evil&state=nope&iss=" + url.QueryEscape(f.srv.URL)
		resp, err := http.Get(cb)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("forged callback status %d, want 400", resp.StatusCode)
		}
		return browser(t)(u)
	}
	if err := Login(loginCtx(t), f.srv.URL, open, store); err != nil {
		t.Fatal(err)
	}
	if tok, _ := store.Token(); tok == nil || tok.AccessToken != "tok-123" {
		t.Errorf("token %+v", tok)
	}
}

func TestLoginTimesOutWithoutCallback(t *testing.T) {
	f := newFakeAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := Login(ctx, f.srv.URL, func(string) error { return nil }, newStore(t))
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenStoreRefusesReadableFile(t *testing.T) {
	store := newStore(t)
	if err := store.SaveToken(&Token{AccessToken: "x", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Token(); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("err = %v, want refusal", err)
	}
	if st := store.Status(); st.State != TokenUnusable || st.Err == nil {
		t.Errorf("status %+v, want unusable", st)
	}
}

func TestTokenStatus(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		expires time.Time
		want    TokenState
	}{
		{"no expiry", time.Time{}, TokenValid},
		{"far", now.Add(72 * time.Hour), TokenValid},
		{"soon", now.Add(2 * time.Hour), TokenExpiringSoon},
		{"expired", now.Add(-time.Second), TokenExpired},
		{"exactly now", now, TokenExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			store.Now = func() time.Time { return now }
			if st := store.Status(); st.State != TokenMissing {
				t.Fatalf("empty store status %v", st.State)
			}
			if err := store.SaveToken(&Token{AccessToken: "x", ExpiresAt: tc.expires}); err != nil {
				t.Fatal(err)
			}
			if st := store.Status(); st.State != tc.want {
				t.Errorf("status %v, want %v", st.State, tc.want)
			}
		})
	}
}

func TestWellKnownInsertsBeforeIssuerPath(t *testing.T) {
	got, _ := wellKnown("https://as.example/tenant/", "oauth-authorization-server")
	if want := "https://as.example/.well-known/oauth-authorization-server/tenant"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
