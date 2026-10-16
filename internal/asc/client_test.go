package asc

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/config"
)

func decodeSegment(t *testing.T, s string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func assertJSON(t *testing.T, got map[string]any, want string) {
	t.Helper()
	var w map[string]any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	g, _ := json.Marshal(got)
	wb, _ := json.Marshal(w)
	if string(g) != string(wb) {
		t.Errorf("got %s, want %s", g, wb)
	}
}

// Port: "makeToken produces an ES256 JWT that verifies with the public key".
func TestTokenVerifies(t *testing.T) {
	key := testKey()
	tok, err := makeToken("ABCDE12345", "issuer", key, time.Unix(1_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	assertJSON(t, decodeSegment(t, parts[0]), `{"alg":"ES256","kid":"ABCDE12345","typ":"JWT"}`)
	assertJSON(t, decodeSegment(t, parts[1]), `{"iss":"issuer","iat":1000,"exp":1600,"aud":"appstoreconnect-v1"}`)
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes; JWS ES256 signatures are raw r||s, not DER", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Error("signature does not verify with the public key")
	}
}

// Port: "individual keys sign with the subject "user" and no issuer".
func TestTokenIndividualKey(t *testing.T) {
	tok, err := makeToken("ABCDE12345", "", testKey(), time.Unix(1_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, decodeSegment(t, strings.Split(tok, ".")[1]), `{"sub":"user","iat":1000,"exp":1600,"aud":"appstoreconnect-v1"}`)
}

func TestTokenCachedAndRefreshedBeforeExpiry(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, list()) })
	ctx := context.Background()
	for range 2 {
		if _, err := f.c.Ping(ctx); err != nil {
			t.Fatal(err)
		}
	}
	f.advance(9 * time.Minute) // inside the refresh margin of a 10-minute token
	if _, err := f.c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].Auth != f.calls[1].Auth {
		t.Error("token was not reused within its lifetime")
	}
	if f.calls[1].Auth == f.calls[2].Auth {
		t.Error("token was not refreshed before expiry")
	}
	claims := decodeSegment(t, strings.Split(strings.TrimPrefix(f.calls[2].Auth, "Bearer "), ".")[1])
	if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat > 20*60 {
		t.Errorf("token lifetime %vs exceeds Apple's 20 minutes", exp-iat)
	}
}

func TestNewLoadsKey(t *testing.T) {
	dir := t.TempDir()
	der, err := x509.MarshalPKCS8PrivateKey(testKey())
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "AuthKey_ABCDE12345.p8")
	if err := os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(config.ASC{KeyID: "ABCDE12345", IssuerID: "issuer", KeyPath: good})
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != DefaultBaseURL || c.key == nil || c.HTTP == nil {
		t.Errorf("client not initialised: %+v", c)
	}

	secret := "-----BEGIN PRIVATE KEY-----\nc2VjcmV0LWtleS1tYXRlcmlhbA==\n-----END PRIVATE KEY-----\n"
	bad := filepath.Join(dir, "bad.p8")
	if err := os.WriteFile(bad, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = New(config.ASC{KeyID: "ABCDE12345", KeyPath: bad})
	if err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Errorf("bad key: %v", err)
	}
	if _, err := New(config.ASC{KeyPath: good}); err == nil {
		t.Error("missing keyId accepted")
	}
	if _, err := New(config.ASC{KeyID: "x", KeyPath: filepath.Join(dir, "missing.p8")}); err == nil {
		t.Error("missing key file accepted")
	}
}

// Port: "apiFailure maps HTTP errors to pipeline exit codes".
func TestErrorMapping(t *testing.T) {
	agreement := fixture(t, "error_agreement.json")
	cases := []struct {
		name    string
		status  int
		body    any
		is      error
		agree   bool
		message string
	}{
		{"auth", 401, appleError("NOT_AUTHORIZED", "Authentication credentials are missing or invalid."), ErrAuth, false, "rejected the API key"},
		{"agreement", 403, agreement, ErrForbidden, true, "agreement"},
		{"role", 403, appleError("FORBIDDEN_ERROR", "This request is forbidden for security reasons"), ErrForbidden, false, "role"},
		{"not found", 404, appleError("NOT_FOUND", "The specified resource does not exist"), ErrNotFound, false, "rejected GET /v1/apps"},
		{"conflict", 409, appleError("ENTITY_ERROR.RELATIONSHIP.INVALID", "already"), ErrConflict, false, "rejected"},
		{"unavailable", 503, appleError("SERVICE_UNAVAILABLE", ""), ErrUnavailable, false, "failed on GET /v1/apps"},
		{"bad request", 400, appleError("PARAMETER_ERROR.INVALID", "bad filter"), nil, false, "PARAMETER_ERROR.INVALID: bad filter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, tc.status, tc.body) })
			_, err := f.c.get(context.Background(), "/v1/apps?limit=1")
			var ae *APIError
			if !errors.As(err, &ae) {
				t.Fatalf("want *APIError, got %v", err)
			}
			if ae.Status != tc.status || ae.Path != "/v1/apps" || len(ae.Errors) == 0 {
				t.Errorf("APIError %+v", ae)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("errors.Is(%v, %v) = false", err, tc.is)
			}
			if ae.Agreement != tc.agree || IsAgreement(err) != tc.agree {
				t.Errorf("agreement = %v, want %v", ae.Agreement, tc.agree)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("message %q lacks %q", err, tc.message)
			}
			for _, other := range []error{ErrAuth, ErrForbidden, ErrNotFound, ErrConflict, ErrUnavailable} {
				if other != tc.is && errors.Is(err, other) {
					t.Errorf("also matches %v", other)
				}
			}
		})
	}
}

func TestUnauthorizedRetriedOnceWithFreshToken(t *testing.T) {
	var n atomic.Int32
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			reply(w, 401, appleError("NOT_AUTHORIZED", "expired"))
			return
		}
		reply(w, 200, list())
	})
	if h, err := f.c.Ping(context.Background()); h != HealthOK {
		t.Fatalf("ping %s %v", h, err)
	}
	if len(f.sleeps) != 0 || n.Load() != 2 {
		t.Errorf("calls %d sleeps %v", n.Load(), f.sleeps)
	}

	// A key that is really rejected fails after the one re-mint, without backoff.
	g := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 401, appleError("NOT_AUTHORIZED", "")) })
	if _, err := g.c.get(context.Background(), "/v1/apps"); !errors.Is(err, ErrAuth) {
		t.Fatal(err)
	}
	if g.count("GET", "/v1/apps") != 2 || len(g.sleeps) != 0 {
		t.Errorf("calls %d sleeps %v", g.count("GET", "/v1/apps"), g.sleeps)
	}
}

// Port of the client half of "finish waits through processing and a full outage": short
// outages are retried inside the client with backoff; a full one surfaces as ErrUnavailable.
func TestRetriesThroughOutage(t *testing.T) {
	var n atomic.Int32
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 3 {
			reply(w, 503, appleError("SERVICE_UNAVAILABLE", ""))
			return
		}
		reply(w, 200, list())
	})
	if _, err := f.c.get(context.Background(), "/v1/apps"); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(f.sleeps) != 3 || f.sleeps[0] != want[0] || f.sleeps[1] != want[1] || f.sleeps[2] != want[2] {
		t.Errorf("backoff %v, want %v", f.sleeps, want)
	}

	full := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 500, nil) })
	_, err := full.c.get(context.Background(), "/v1/apps")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("full outage: %v", err)
	}
	if got := full.count("GET", "/v1/apps"); got != defaultAttempts {
		t.Errorf("attempts %d, want %d", got, defaultAttempts)
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	var n atomic.Int32
	var f *fake
	f = newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			reply(w, 429, appleError("RATE_LIMIT_EXCEEDED", ""))
		case 2:
			w.Header().Set("Retry-After", f.clock().Add(90*time.Second).UTC().Format(http.TimeFormat))
			reply(w, 429, appleError("RATE_LIMIT_EXCEEDED", ""))
		case 3:
			w.Header().Set("Retry-After", "86400")
			reply(w, 503, nil)
		default:
			reply(w, 200, list())
		}
	})
	if _, err := f.c.get(context.Background(), "/v1/apps"); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{7 * time.Second, 90 * time.Second, maxRetryWait}
	if len(f.sleeps) != 3 || f.sleeps[0] != want[0] || f.sleeps[1] != want[1] || f.sleeps[2] != want[2] {
		t.Errorf("sleeps %v, want %v", f.sleeps, want)
	}
}

func TestNetworkErrorAndUnreadableBodyRetried(t *testing.T) {
	var n atomic.Int32
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		case 2:
			reply(w, 200, "<html>gateway</html>")
		default:
			reply(w, 200, list())
		}
	})
	if _, err := f.c.get(context.Background(), "/v1/apps"); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 {
		t.Errorf("requests %d, want 3", n.Load())
	}

	down := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	})
	if _, err := down.c.get(context.Background(), "/v1/apps"); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("network down: %v", err)
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 503, nil) })
	ctx, cancel := context.WithCancel(context.Background())
	f.c.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	if _, err := f.c.get(ctx, "/v1/apps"); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
	if f.count("", "/v1/apps") != 1 {
		t.Errorf("requests after cancel: %d", f.count("", "/v1/apps"))
	}
}

func TestPaginationFollowsNextAndStaysOnHost(t *testing.T) {
	var base string
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			reply(w, 200, map[string]any{
				"data":  []any{buildJSON("b1", "9", "VALID", ""), buildJSON("b2", "202610161458", "VALID", "")},
				"links": map[string]string{"next": base + "/v1/builds?cursor=Ag&limit=2"},
			})
			return
		}
		reply(w, 200, list(buildJSON("b3", "10", "PROCESSING", ""), buildJSON("b4", "2", "FAILED", "")))
	})
	base = f.c.BaseURL
	num, ok, err := f.c.LatestBuildNumber(context.Background(), "app1", "0.1.0")
	if err != nil || !ok || num != "202610161458" {
		t.Fatalf("latest = %q %v %v", num, ok, err)
	}
	q := f.calls[0].Query
	for _, want := range []string{"filter%5Bapp%5D=app1", "filter%5BpreReleaseVersion.version%5D=0.1.0", "filter%5BprocessingState%5D=PROCESSING%2CFAILED%2CINVALID%2CVALID"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %s lacks %s", q, want)
		}
	}
	if len(f.calls) != 2 {
		t.Errorf("pages fetched: %d", len(f.calls))
	}

	evil := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"data": []any{}, "links": map[string]string{"next": "https://evil.example/v1/builds?cursor=Ag"}})
	})
	if _, _, err := evil.c.LatestBuildNumber(context.Background(), "app1", "0.1.0"); err == nil || !strings.Contains(err.Error(), "refusing next link") {
		t.Errorf("foreign next link: %v", err)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":     0,
		"3":    3 * time.Second,
		"-1":   0,
		"soon": 0,
		now.Add(time.Minute).Format(http.TimeFormat):  time.Minute,
		now.Add(-time.Minute).Format(http.TimeFormat): 0,
	} {
		if got := retryAfter(v, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}
