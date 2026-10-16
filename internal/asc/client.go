// Package asc is eve's App Store Connect API client: ES256 tokens from the standard library,
// build and upload lookups, the processing and TestFlight verdicts, delivery reconciliation
// (PLAN.md §5.5) and internal-group distribution (PLAN.md §6.3). It is a port of
// bodydiff-testflight's scripts/app-store-connect.mjs.
package asc

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/self-deprecated/eve/internal/config"
)

// DefaultBaseURL is the App Store Connect API.
const DefaultBaseURL = "https://api.appstoreconnect.apple.com"

// Platform is the preReleaseVersion and upload platform eve builds for.
const Platform = "IOS"

const (
	defaultAttempts = 4
	defaultBackoff  = 2 * time.Second
	maxRetryWait    = 2 * time.Minute
	maxPages        = 50
	maxBody         = 16 << 20
)

// Client calls the App Store Connect API. The zero value is not usable; build one with New.
// It is safe for concurrent use.
type Client struct {
	BaseURL  string
	KeyID    string
	IssuerID string // empty for an individual key
	key      *ecdsa.PrivateKey
	HTTP     *http.Client
	// Now is the clock for token timestamps and polling windows; nil means time.Now.
	Now func() time.Time
	// Attempts bounds tries per request for retryable failures; 0 means 4.
	Attempts int
	// Backoff is the first retry delay, doubled per attempt unless Retry-After says otherwise;
	// 0 means 2 s.
	Backoff time.Duration

	// sleep waits between retries and polls; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error

	mu     sync.Mutex
	tok    string
	tokExp time.Time
}

// New builds a client from the server's ASC key configuration, loading the .p8 key.
func New(cfg config.ASC) (*Client, error) {
	if cfg.KeyID == "" {
		return nil, errors.New("asc: keyId is required")
	}
	if cfg.KeyPath == "" {
		return nil, errors.New("asc: keyPath is required")
	}
	key, err := loadKey(config.ExpandHome(cfg.KeyPath))
	if err != nil {
		return nil, err
	}
	return &Client{
		BaseURL:  DefaultBaseURL,
		KeyID:    cfg.KeyID,
		IssuerID: cfg.IssuerID,
		key:      key,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// resource is one JSON:API resource object.
type resource struct {
	Type          string                  `json:"type"`
	ID            string                  `json:"id"`
	Attributes    json.RawMessage         `json:"attributes"`
	Relationships map[string]relationship `json:"relationships"`
}

type relationship struct {
	Data json.RawMessage `json:"data"`
}

// ref returns the single linkage of a to-one relationship, if any.
func (r resource) ref(name string) (typ, id string, ok bool) {
	rel, found := r.Relationships[name]
	if !found || len(rel.Data) == 0 || string(rel.Data) == "null" {
		return "", "", false
	}
	var l struct{ Type, ID string }
	if json.Unmarshal(rel.Data, &l) != nil || l.ID == "" {
		return "", "", false
	}
	return l.Type, l.ID, true
}

// document is a JSON:API response.
type document struct {
	Data     json.RawMessage `json:"data"`
	Included []resource      `json:"included"`
	Links    struct {
		Next string `json:"next"`
	} `json:"links"`
}

// list decodes data as an array of resources.
func (d *document) list() ([]resource, error) {
	if len(d.Data) == 0 || string(d.Data) == "null" {
		return nil, nil
	}
	var out []resource
	if err := json.Unmarshal(d.Data, &out); err != nil {
		return nil, fmt.Errorf("asc: decode data: %w", err)
	}
	return out, nil
}

// one decodes data as a single resource; nil when data is null.
func (d *document) one() (*resource, error) {
	if len(d.Data) == 0 || string(d.Data) == "null" {
		return nil, nil
	}
	var out resource
	if err := json.Unmarshal(d.Data, &out); err != nil {
		return nil, fmt.Errorf("asc: decode data: %w", err)
	}
	return &out, nil
}

// get fetches one page.
func (c *Client) get(ctx context.Context, path string) (*document, error) {
	var doc document
	if err := c.do(ctx, http.MethodGet, path, nil, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// getAll follows links.next from path, calling fn per page until it returns stop or the pages
// run out. Next links must stay on BaseURL so the token never leaves App Store Connect.
func (c *Client) getAll(ctx context.Context, path string, fn func(*document) (stop bool, err error)) error {
	for range maxPages {
		doc, err := c.get(ctx, path)
		if err != nil {
			return err
		}
		stop, err := fn(doc)
		if err != nil || stop || doc.Links.Next == "" {
			return err
		}
		next, ok := strings.CutPrefix(doc.Links.Next, strings.TrimRight(c.BaseURL, "/"))
		if !ok || !strings.HasPrefix(next, "/") {
			return fmt.Errorf("asc: refusing next link outside %s", c.BaseURL)
		}
		path = next
	}
	return fmt.Errorf("asc: more than %d pages from %s", maxPages, path)
}

// do sends one request with bounded retries and decodes a JSON response into out (which may
// be nil). Network errors, interrupted or unreadable successes, 429 and 5xx are retried with
// backoff, honouring Retry-After. A retried POST can meet its own earlier success; callers
// treat the resulting 409 as done. A 401 is retried once with a freshly minted token.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	where := method + " " + strings.SplitN(path, "?", 2)[0]
	attempts := c.Attempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	backoff := c.Backoff
	if backoff <= 0 {
		backoff = defaultBackoff
	}
	reminted := false
	for attempt := 1; ; attempt++ {
		status, header, data, err := c.once(ctx, method, path, payload)
		var retryErr error
		var delay time.Duration
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			retryErr = fmt.Errorf("%w: %s is unreachable: %w", ErrUnavailable, where, err)
		case status == http.StatusUnauthorized && !reminted:
			// A stale or skewed cached token; mint a new one and try once more.
			reminted = true
			c.dropToken()
			attempt--
			continue
		case status == http.StatusTooManyRequests || status >= 500:
			retryErr = newAPIError(method, path, status, decodeErrors(data))
			delay = retryAfter(header.Get("Retry-After"), c.now())
		case status >= 200 && status < 300:
			if out == nil || len(bytes.TrimSpace(data)) == 0 {
				return nil
			}
			if jerr := json.Unmarshal(data, out); jerr == nil {
				return nil
			}
			retryErr = fmt.Errorf("%w: unreadable response on %s", ErrUnavailable, where)
		default:
			return newAPIError(method, path, status, decodeErrors(data))
		}
		if attempt >= attempts {
			return retryErr
		}
		if delay == 0 {
			delay = backoff << (attempt - 1)
		}
		if err := c.wait(ctx, min(delay, maxRetryWait)); err != nil {
			return err
		}
	}
}

// once performs a single HTTP exchange.
func (c *Client) once(ctx context.Context, method, path string, payload []byte) (int, http.Header, []byte, error) {
	tok, err := c.token()
	if err != nil {
		return 0, nil, nil, err
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, data, nil
}

func decodeErrors(data []byte) []ErrorItem {
	var body struct {
		Errors []ErrorItem `json:"errors"`
	}
	_ = json.Unmarshal(data, &body)
	return body.Errors
}

// retryAfter parses a Retry-After header (seconds or an HTTP date); 0 when absent or invalid.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
