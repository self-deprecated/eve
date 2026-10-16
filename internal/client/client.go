// Package client talks to an eve server (PLAN.md §11). It is used by the CLI on every machine.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
)

// Client is an authenticated API client.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// LocalServer is the server's default loopback address on the build host.
const LocalServer = "http://127.0.0.1:4790"

// LocalOwnerTokenPath is the owner token `eve serve` creates on the build host.
func LocalOwnerTokenPath() string {
	if d := os.Getenv("EVE_DATA_DIR"); d != "" {
		return filepath.Join(d, "token")
	}
	return config.ExpandHome("~/.local/share/eve/token")
}

// TokenPath is where the client keeps its token (0600), next to client.yml.
func TokenPath() string { return filepath.Join(config.ClientDir(), "token") }

// FromConfig builds a client from client.yml and the token file ($EVE_TOKEN overrides).
func FromConfig() (*Client, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, err
	}
	tok := os.Getenv("EVE_TOKEN")
	if tok == "" {
		if b, err := os.ReadFile(TokenPath()); err == nil {
			tok = strings.TrimSpace(string(b))
		}
	}
	// On the build host itself, with no login: the local server and its owner token.
	if cfg.Server == "" && tok == "" {
		if b, err := os.ReadFile(LocalOwnerTokenPath()); err == nil {
			return New(LocalServer, strings.TrimSpace(string(b))), nil
		}
	}
	if cfg.Server == "" {
		return nil, errors.New("no eve server configured; run `eve login <url>`")
	}
	if tok == "" {
		return nil, fmt.Errorf("no token in %s; run `eve login %s`", TokenPath(), cfg.Server)
	}
	return New(cfg.Server, tok), nil
}

// New returns a client for baseURL.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 0}}
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%s)", e.Msg, e.Code)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg)
}

// ErrUnreachable wraps connection failures with a hint about Quartz's state.
type ErrUnreachable struct{ Err error }

func (e *ErrUnreachable) Error() string {
	return fmt.Sprintf("eve server unreachable (%v): is Quartz on, logged in at the console, and is eve serve running?", e.Err)
}
func (e *ErrUnreachable) Unwrap() error { return e.Err }

func (c *Client) req(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ErrUnreachable{err}
	}
	return resp, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := c.req(cctx, method, path, body, ct)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return decode(resp, out)
}

func decode(resp *http.Response, out any) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e api.ErrorBody
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return &APIError{Status: resp.StatusCode, Code: e.Code, Msg: e.Error}
		}
		return &APIError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = string(b)
		return nil
	}
	return json.Unmarshal(b, out)
}

// Version returns the server's version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct{ Version string }
	err := c.doJSON(ctx, "GET", "/v1/version", nil, &v)
	return v.Version, err
}

// HasSource reports whether the server already has an upload with this digest.
func (c *Client) HasSource(ctx context.Context, sha string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.req(cctx, "HEAD", "/v1/sources/"+sha, nil, "")
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, &APIError{Status: resp.StatusCode, Msg: "checking source"}
}

// PutSource uploads a packed source archive.
func (c *Client) PutSource(ctx context.Context, sha string, r io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", c.BaseURL+"/v1/sources/"+sha, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/zstd")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &ErrUnreachable{err}
	}
	defer func() { _ = resp.Body.Close() }()
	return decode(resp, nil)
}

// Submit creates a job.
func (c *Client) Submit(ctx context.Context, req api.JobRequest) (api.JobCreated, error) {
	var out api.JobCreated
	err := c.doJSON(ctx, "POST", "/v1/jobs", req, &out)
	return out, err
}

// Job returns a job's result document.
func (c *Client) Job(ctx context.Context, id string) (api.Result, error) {
	var r api.Result
	err := c.doJSON(ctx, "GET", "/v1/jobs/"+url.PathEscape(id), nil, &r)
	return r, err
}

// Jobs lists jobs.
func (c *Client) Jobs(ctx context.Context, app string, limit int) ([]api.Result, error) {
	q := url.Values{}
	if app != "" {
		q.Set("app", app)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []api.Result
	err := c.doJSON(ctx, "GET", "/v1/jobs?"+q.Encode(), nil, &out)
	return out, err
}

// Log returns a phase's log; errorsOnly asks the server for the relevant lines only.
func (c *Client) Log(ctx context.Context, id, phase string, errorsOnly bool, tail int) (string, error) {
	q := url.Values{}
	if errorsOnly {
		q.Set("errors", "1")
	}
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	var s string
	err := c.doJSON(ctx, "GET", "/v1/jobs/"+url.PathEscape(id)+"/logs/"+url.PathEscape(phase)+"?"+q.Encode(), nil, &s)
	return s, err
}

// Logs lists the phases that have logs.
func (c *Client) Logs(ctx context.Context, id string) ([]string, error) {
	var out []string
	err := c.doJSON(ctx, "GET", "/v1/jobs/"+url.PathEscape(id)+"/logs", nil, &out)
	return out, err
}

// Size returns the full size report.
func (c *Client) Size(ctx context.Context, id string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.doJSON(ctx, "GET", "/v1/jobs/"+url.PathEscape(id)+"/size", nil, &out)
	return out, err
}

// Deliveries lists a job's notification deliveries.
func (c *Client) Deliveries(ctx context.Context, id string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.doJSON(ctx, "GET", "/v1/jobs/"+url.PathEscape(id)+"/deliveries", nil, &out)
	return out, err
}

// Cancel cancels a job.
func (c *Client) Cancel(ctx context.Context, id string, force bool) (api.Result, error) {
	path := "/v1/jobs/" + url.PathEscape(id) + "/cancel"
	if force {
		path += "?force=1"
	}
	var r api.Result
	err := c.doJSON(ctx, "POST", path, nil, &r)
	return r, err
}

// Confirm accepts a size-guard pause.
func (c *Client) Confirm(ctx context.Context, id, reason string) (api.Result, error) {
	var r api.Result
	err := c.doJSON(ctx, "POST", "/v1/jobs/"+url.PathEscape(id)+"/confirm", map[string]string{"reason": reason}, &r)
	return r, err
}

// Reject refuses a size-guard pause.
func (c *Client) Reject(ctx context.Context, id string) (api.Result, error) {
	var r api.Result
	err := c.doJSON(ctx, "POST", "/v1/jobs/"+url.PathEscape(id)+"/reject", nil, &r)
	return r, err
}

// SourceStats returns the app's last accepted source stats (nil if none).
func (c *Client) SourceStats(ctx context.Context, app string) (*api.SourceStats, error) {
	var out *api.SourceStats
	err := c.doJSON(ctx, "GET", "/v1/apps/"+url.PathEscape(app)+"/source-stats", nil, &out)
	return out, err
}

// Raw performs a GET and returns the JSON body (apps, builds, baseline, doctor).
func (c *Client) Raw(ctx context.Context, path string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.doJSON(ctx, "GET", path, nil, &out)
	return out, err
}

// PutJSON performs a PUT with a JSON body.
func (c *Client) PutJSON(ctx context.Context, path string, in any) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.doJSON(ctx, "PUT", path, in, &out)
	return out, err
}

// Wait follows a job's events until it is terminal or awaiting confirmation, reconnecting with
// Last-Event-ID after network errors. onEvent (optional) sees every event. It returns the final
// result document; ctx's deadline bounds the wait.
func (c *Client) Wait(ctx context.Context, id string, onEvent func(api.Event)) (api.Result, error) {
	var last int64
	backoff := time.Second
	for {
		done, err := c.follow(ctx, id, &last, onEvent)
		if done {
			return c.Job(context.WithoutCancel(ctx), id)
		}
		if ctx.Err() != nil {
			r, jerr := c.Job(context.WithoutCancel(ctx), id)
			if jerr != nil {
				return r, ctx.Err()
			}
			return r, ctx.Err()
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status < 500 {
			return api.Result{}, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// follow reads one SSE connection. It reports done when a terminal or awaiting_confirmation
// event arrived.
func (c *Client) follow(ctx context.Context, id string, last *int64, onEvent func(api.Event)) (bool, error) {
	r, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/v1/jobs/"+url.PathEscape(id)+"/events", nil)
	if err != nil {
		return false, err
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.Header.Set("Accept", "text/event-stream")
	if *last > 0 {
		r.Header.Set("Last-Event-ID", strconv.FormatInt(*last, 10))
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return false, &ErrUnreachable{err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, decode(resp, nil)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		case line == "" && data.Len() > 0:
			var ev api.Event
			if err := json.Unmarshal([]byte(data.String()), &ev); err == nil {
				*last = ev.Seq
				if onEvent != nil {
					onEvent(ev)
				}
				if ev.Kind == api.EventTransition && (ev.State.Terminal() || ev.State == api.StateAwaitingConfirmation) {
					return true, nil
				}
			}
			data.Reset()
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return false, &ErrUnreachable{err}
	}
	// The server closes the stream after a terminal event; a clean close without one means the
	// subscriber fell behind or the server restarted: reconnect.
	return false, io.ErrUnexpectedEOF
}
