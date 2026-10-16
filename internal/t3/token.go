package t3

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ExpiringSoon is how close to its expiry a token counts as expiring soon. It matches the
// outbox's 24 h retry window: a token that expires within it may strand a delivery.
const ExpiringSoon = 24 * time.Hour

// Token is a stored T3 access token. T3 advertises no refresh-token grant, so when it expires
// the user runs `eve t3 login` again.
type Token struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"` // zero when the server gave no expires_in
	Scope       string    `json:"scope,omitempty"`
	ObtainedAt  time.Time `json:"obtained_at"`
	Issuer      string    `json:"issuer"`
	Resource    string    `json:"resource"`
}

// Registration is Eve's dynamic client registration with T3's authorization server, kept so a
// later login can reuse it instead of registering a new client each time.
type Registration struct {
	Issuer      string    `json:"issuer"`
	ClientID    string    `json:"client_id"`
	RedirectURI string    `json:"redirect_uri"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"` // client_secret_expires_at, if any
}

// storeFile is the on-disk layout of a TokenStore.
type storeFile struct {
	Registration *Registration `json:"registration,omitempty"`
	Token        *Token        `json:"token,omitempty"`
}

// TokenStore persists the registration and access token in one JSON file (PLAN.md §8.1:
// ~/.local/share/eve/t3-token). The file is written atomically with mode 0600, and a file that
// group or other can read is refused rather than trusted.
type TokenStore struct {
	Path string
	// Now is the clock for Status; nil means time.Now.
	Now func() time.Time
}

// ErrNoToken is returned when the store holds no access token.
var ErrNoToken = errors.New("no T3 token; run `eve t3 login` on the server")

func (s *TokenStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// load reads the file. A missing file is an empty store.
func (s *TokenStore) load() (storeFile, error) {
	var f storeFile
	fh, err := os.Open(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	defer func() { _ = fh.Close() }()
	// Check the open file, not the path, so a swap between stat and read cannot slip past.
	fi, err := fh.Stat()
	if err != nil {
		return f, err
	}
	if !fi.Mode().IsRegular() {
		return f, fmt.Errorf("%s: not a regular file", s.Path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return f, fmt.Errorf("%s: mode %04o is readable by group or other; chmod 600 it or log in again", s.Path, perm)
	}
	if err := json.NewDecoder(fh).Decode(&f); err != nil {
		return f, fmt.Errorf("%s: %w", s.Path, err)
	}
	return f, nil
}

// save writes f atomically: a 0600 temporary file in the same directory, synced, then renamed.
func (s *TokenStore) save(f storeFile) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".t3-token-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// Token returns the stored access token, or ErrNoToken.
func (s *TokenStore) Token() (*Token, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	if f.Token == nil || f.Token.AccessToken == "" {
		return nil, ErrNoToken
	}
	return f.Token, nil
}

// Registration returns the stored client registration, or nil if there is none.
func (s *TokenStore) Registration() (*Registration, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Registration, nil
}

// SaveRegistration replaces the stored registration, keeping the token.
func (s *TokenStore) SaveRegistration(r *Registration) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	f.Registration = r
	return s.save(f)
}

// SaveToken replaces the stored token, keeping the registration.
func (s *TokenStore) SaveToken(t *Token) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	f.Token = t
	return s.save(f)
}

// TokenState summarizes whether Eve can currently deliver to T3.
type TokenState string

// Token states reported by Status.
const (
	TokenValid        TokenState = "valid"
	TokenExpiringSoon TokenState = "expiring_soon"
	TokenExpired      TokenState = "expired"
	TokenMissing      TokenState = "missing"
	TokenUnusable     TokenState = "unusable" // unreadable or insecure file; see Err
)

// TokenStatus is the store's state for `eve doctor` and submit-time warnings.
type TokenStatus struct {
	State     TokenState
	ExpiresAt time.Time // zero if unknown or missing
	Scope     string
	Err       error // set when State is TokenUnusable
}

// Status reports whether the stored token is usable. A token without an expiry is valid until
// T3 rejects it; delivery then fails with ErrUnauthorized.
func (s *TokenStore) Status() TokenStatus {
	t, err := s.Token()
	switch {
	case errors.Is(err, ErrNoToken):
		return TokenStatus{State: TokenMissing}
	case err != nil:
		return TokenStatus{State: TokenUnusable, Err: err}
	}
	st := TokenStatus{State: TokenValid, ExpiresAt: t.ExpiresAt, Scope: t.Scope}
	if !t.ExpiresAt.IsZero() {
		now := s.now()
		switch {
		case !now.Before(t.ExpiresAt):
			st.State = TokenExpired
		case t.ExpiresAt.Sub(now) < ExpiringSoon:
			st.State = TokenExpiringSoon
		}
	}
	return st
}
