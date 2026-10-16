// Package t3 delivers Eve notifications into T3 Code threads through T3's local MCP endpoint
// (PLAN.md §8.1, spike/m0-wakeup/RESULT.md).
//
// T3 protects /mcp with a standard MCP OAuth authorization server on the same loopback origin.
// Login performs the one-time interactive flow: protected-resource and authorization-server
// metadata discovery (RFC 9728, RFC 8414), dynamic registration of a public client (RFC 7591),
// and an authorization-code grant with PKCE S256 (RFC 7636) through a loopback redirect
// (RFC 8252), validating state and the iss response parameter (RFC 9207). The resulting bearer
// token is kept in a TokenStore. T3 advertises no refresh grant, so an expired or revoked token
// needs the user to log in again; Client reports that as ErrUnauthorized.
package t3

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Scope is the scope Eve requests: enough to send messages into threads.
const Scope = "orchestration:operate"

// ClientName is the client_name Eve registers under; T3's consent screen shows it.
const ClientName = "eve"

// oauthHTTP is used for metadata, registration and token requests, all loopback and small.
var oauthHTTP = &http.Client{Timeout: 30 * time.Second}

// protectedResource is the subset of RFC 9728 metadata Eve reads.
type protectedResource struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// authServer is the subset of RFC 8414 metadata Eve reads.
type authServer struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	ISSParameterSupported         bool     `json:"authorization_response_iss_parameter_supported"`
}

// Login runs the interactive OAuth flow against the T3 server at baseURL and stores the token.
// open is handed the authorization URL; the CLI prints it and tries to open a browser, and
// should return nil if printing succeeded even when the browser could not be launched. Login
// waits for the browser redirect until ctx is done.
func Login(ctx context.Context, baseURL string, open func(url string) error, store *TokenStore) error {
	base := strings.TrimRight(baseURL, "/")
	pr, as, err := discover(ctx, base)
	if err != nil {
		return err
	}

	ln, reg, err := listenAndRegister(ctx, as, store)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()

	verifier, err := randomString(32)
	if err != nil {
		return err
	}
	state, err := randomString(16)
	if err != nil {
		return err
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {reg.ClientID},
		"redirect_uri":          {reg.RedirectURI},
		"scope":                 {Scope},
		"state":                 {state},
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {pr.Resource},
	}
	authURL, err := withQuery(as.AuthorizationEndpoint, q)
	if err != nil {
		return err
	}

	results := make(chan callbackResult, 1)
	srv := &http.Server{
		Handler:           callbackHandler(state, as, results),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }() // returns ErrServerClosed on Shutdown
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx) // best effort; the listener is closed regardless
	}()

	if err := open(authURL); err != nil {
		return fmt.Errorf("open authorization URL: %w", err)
	}

	var res callbackResult
	select {
	case <-ctx.Done():
		return fmt.Errorf("waiting for the T3 authorization redirect: %w", ctx.Err())
	case res = <-results:
	}
	if res.err != nil {
		return res.err
	}

	tok, err := exchange(ctx, as, reg, res.code, verifier, pr.Resource)
	if errors.Is(err, errInvalidClient) {
		// T3 forgot the client (e.g. its state was reset): drop it so the next login registers.
		_ = store.SaveRegistration(nil)
	}
	if err != nil {
		return err
	}
	tok.Issuer, tok.Resource = as.Issuer, pr.Resource
	return store.SaveToken(tok)
}

// discover fetches the protected-resource metadata for <base>/mcp and its authorization
// server's metadata, and checks they describe the server Eve was pointed at.
func discover(ctx context.Context, base string) (*protectedResource, *authServer, error) {
	wantResource := base + "/mcp"
	var pr protectedResource
	// RFC 9728 §3.1: the path-suffixed form first, then the root form that T3 also serves.
	err := getJSON(ctx, base+"/.well-known/oauth-protected-resource/mcp", &pr)
	if err != nil {
		if err2 := getJSON(ctx, base+"/.well-known/oauth-protected-resource", &pr); err2 != nil {
			return nil, nil, fmt.Errorf("T3 protected-resource metadata: %w", errors.Join(err, err2))
		}
	}
	if pr.Resource != wantResource {
		return nil, nil, fmt.Errorf("T3 protected-resource metadata names resource %q, want %q", pr.Resource, wantResource)
	}
	if len(pr.AuthorizationServers) == 0 {
		return nil, nil, errors.New("T3 protected-resource metadata lists no authorization server")
	}
	if len(pr.ScopesSupported) > 0 && !slices.Contains(pr.ScopesSupported, Scope) {
		return nil, nil, fmt.Errorf("T3 does not offer scope %s", Scope)
	}

	issuer := strings.TrimRight(pr.AuthorizationServers[0], "/")
	metaURL, err := wellKnown(issuer, "oauth-authorization-server")
	if err != nil {
		return nil, nil, err
	}
	var as authServer
	if err := getJSON(ctx, metaURL, &as); err != nil {
		return nil, nil, fmt.Errorf("T3 authorization-server metadata: %w", err)
	}
	// RFC 8414 §3.3: the issuer must be the one the metadata was fetched for.
	if strings.TrimRight(as.Issuer, "/") != issuer {
		return nil, nil, fmt.Errorf("T3 authorization server issuer %q does not match %q", as.Issuer, issuer)
	}
	if !slices.Contains(as.CodeChallengeMethodsSupported, "S256") {
		return nil, nil, errors.New("T3 authorization server does not support PKCE S256")
	}
	if as.AuthorizationEndpoint == "" || as.TokenEndpoint == "" || as.RegistrationEndpoint == "" {
		return nil, nil, errors.New("T3 authorization server metadata lacks an authorize, token or registration endpoint")
	}
	return &pr, &as, nil
}

// wellKnown builds an RFC 8414 metadata URL: the well-known segment goes between the host and
// any issuer path.
func wellKnown(issuer, name string) (string, error) {
	u, err := url.Parse(issuer)
	if err != nil {
		return "", fmt.Errorf("issuer %q: %w", issuer, err)
	}
	u.Path = "/.well-known/" + name + strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// listenAndRegister opens the loopback redirect listener and returns a registration whose
// redirect URI points at it. A stored registration for the same issuer is reused when its port
// is free, because authorization servers may match redirect URIs including the port.
func listenAndRegister(ctx context.Context, as *authServer, store *TokenStore) (net.Listener, *Registration, error) {
	old, err := store.Registration()
	if err != nil {
		return nil, nil, err
	}
	if old != nil && old.Issuer == strings.TrimRight(as.Issuer, "/") &&
		(old.ExpiresAt.IsZero() || time.Now().Before(old.ExpiresAt)) {
		if u, err := url.Parse(old.RedirectURI); err == nil && u.Hostname() == "127.0.0.1" {
			if ln, err := net.Listen("tcp", u.Host); err == nil {
				return ln, old, nil
			}
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("loopback listener for the OAuth redirect: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	reg, err := register(ctx, as, "http://127.0.0.1:"+strconv.Itoa(port)+"/callback")
	if err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	if err := store.SaveRegistration(reg); err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	return ln, reg, nil
}

// register performs RFC 7591 dynamic registration of a public client.
func register(ctx context.Context, as *authServer, redirect string) (*Registration, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                ClientName,
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      Scope,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, as.RegistrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	var out struct {
		ClientID              string   `json:"client_id"`
		RedirectURIs          []string `json:"redirect_uris"`
		ClientSecretExpiresAt int64    `json:"client_secret_expires_at"`
	}
	if err := doJSON(req, &out); err != nil {
		return nil, fmt.Errorf("register eve with T3: %w", err)
	}
	if out.ClientID == "" {
		return nil, errors.New("register eve with T3: response has no client_id")
	}
	if len(out.RedirectURIs) > 0 && !slices.Contains(out.RedirectURIs, redirect) {
		return nil, fmt.Errorf("register eve with T3: server changed the redirect URI to %v", out.RedirectURIs)
	}
	reg := &Registration{Issuer: strings.TrimRight(as.Issuer, "/"), ClientID: out.ClientID, RedirectURI: redirect}
	if out.ClientSecretExpiresAt > 0 {
		reg.ExpiresAt = time.Unix(out.ClientSecretExpiresAt, 0)
	}
	return reg, nil
}

type callbackResult struct {
	code string
	err  error
}

// callbackHandler serves the redirect URI. Only the first well-formed callback is delivered;
// anything else (favicon requests, stray hits) gets a 404 and is ignored.
func callbackHandler(state string, as *authServer, results chan<- callbackResult) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		res := callbackResult{code: q.Get("code")}
		switch {
		case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1:
			// A mismatched state is either a stale tab or a forgery; do not consume the flow.
			http.Error(w, "state mismatch; restart `eve t3 login`", http.StatusBadRequest)
			return
		case as.ISSParameterSupported && strings.TrimRight(q.Get("iss"), "/") != strings.TrimRight(as.Issuer, "/"):
			res.err = fmt.Errorf("authorization response iss %q does not match issuer %q", q.Get("iss"), as.Issuer)
		case q.Get("error") != "":
			res.err = fmt.Errorf("T3 denied authorization: %s %s", q.Get("error"), q.Get("error_description"))
		case res.code == "":
			res.err = errors.New("authorization response has no code")
		}
		if res.err != nil {
			http.Error(w, res.err.Error(), http.StatusBadRequest)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, "eve is now authorized to message T3 threads. You can close this tab.\n")
		}
		select {
		case results <- res:
		default: // a result is already pending
		}
	})
	return mux
}

// errInvalidClient marks a token-endpoint invalid_client error.
var errInvalidClient = errors.New("invalid_client")

// exchange redeems the authorization code at the token endpoint.
func exchange(ctx context.Context, as *authServer, reg *Registration, code, verifier, resource string) (*Token, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {reg.RedirectURI},
		"client_id":     {reg.ClientID},
		"code_verifier": {verifier},
		"resource":      {resource},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, as.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	now := time.Now()
	if err := doJSON(req, &out); err != nil {
		return nil, fmt.Errorf("T3 token exchange: %w", err)
	}
	if out.AccessToken == "" {
		return nil, errors.New("T3 token exchange: response has no access_token")
	}
	if !strings.EqualFold(out.TokenType, "bearer") {
		return nil, fmt.Errorf("T3 token exchange: token_type %q, want Bearer", out.TokenType)
	}
	t := &Token{AccessToken: out.AccessToken, TokenType: out.TokenType, Scope: out.Scope, ObtainedAt: now.UTC()}
	if t.Scope == "" {
		t.Scope = Scope // RFC 6749 §5.1: omitted means as requested
	}
	if out.ExpiresIn > 0 {
		t.ExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Second).UTC()
	}
	return t, nil
}

// getJSON GETs u and decodes a 200 JSON body into v.
func getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	return doJSON(req, v)
}

// doJSON sends req and decodes a 2xx JSON body into v. OAuth error bodies become errors.
func doJSON(req *http.Request, v any) error {
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var oe struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(body, &oe) == nil && oe.Error != "" {
			err := fmt.Errorf("%s: %s %s", req.URL.Path, oe.Error, oe.Description)
			if oe.Error == "invalid_client" {
				err = fmt.Errorf("%w: %w", errInvalidClient, err)
			}
			return err
		}
		return fmt.Errorf("%s: HTTP %d", req.URL.Path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: %w", req.URL.Path, err)
	}
	return nil
}

// withQuery appends q to an endpoint URL that may already carry a query.
func withQuery(endpoint string, q url.Values) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("authorization endpoint %q: %w", endpoint, err)
	}
	existing := u.Query()
	for k, vs := range q {
		existing[k] = vs
	}
	u.RawQuery = existing.Encode()
	return u.String(), nil
}

// randomString returns n random bytes, base64url-encoded without padding. For n=32 this is a
// 43-character PKCE verifier (RFC 7636 §4.1).
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// challenge is the S256 code challenge for verifier.
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
