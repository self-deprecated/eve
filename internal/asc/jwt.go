package asc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"
)

// tokenLifetime is how long a minted token is valid. Apple accepts at most 20 minutes.
const tokenLifetime = 10 * time.Minute

// tokenRefresh is how close to expiry a cached token is replaced.
const tokenRefresh = 2 * time.Minute

// loadKey reads an App Store Connect .p8 key (PKCS#8 PEM, EC P-256). Errors never quote the
// file's contents.
func loadKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("asc: read key: %w", err)
	}
	return parseKey(data)
}

func parseKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("asc: key file is not PEM")
	}
	var parsed any
	var err error
	switch block.Type {
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if err != nil {
		return nil, errors.New("asc: key file is not a PKCS#8 EC key")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("asc: key file is not a P-256 EC key")
	}
	return key, nil
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	Iss string `json:"iss,omitempty"`
	Sub string `json:"sub,omitempty"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Aud string `json:"aud"`
}

// makeToken signs an ES256 JWT for the App Store Connect API. Team keys name their issuer;
// individual keys (no issuer) use the subject "user" instead. The signature is the raw
// IEEE P1363 r||s form JWS requires, not DER.
func makeToken(keyID, issuerID string, key *ecdsa.PrivateKey, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header, err := json.Marshal(jwtHeader{Alg: "ES256", Kid: keyID, Typ: "JWT"})
	if err != nil {
		return "", err
	}
	claims := jwtClaims{Iat: now.Unix(), Exp: now.Add(tokenLifetime).Unix(), Aud: "appstoreconnect-v1"}
	if issuerID != "" {
		claims.Iss = issuerID
	} else {
		claims.Sub = "user"
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("asc: sign token: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + enc.EncodeToString(sig), nil
}

// token returns a cached token, minting a new one when none is cached or it is close to
// expiry.
func (c *Client) token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.tok != "" && now.Before(c.tokExp.Add(-tokenRefresh)) {
		return c.tok, nil
	}
	tok, err := makeToken(c.KeyID, c.IssuerID, c.key, now)
	if err != nil {
		return "", err
	}
	c.tok, c.tokExp = tok, now.Add(tokenLifetime)
	return tok, nil
}

// dropToken forgets the cached token so the next request mints a fresh one.
func (c *Client) dropToken() {
	c.mu.Lock()
	c.tok = ""
	c.mu.Unlock()
}
