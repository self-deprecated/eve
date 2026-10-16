package t3

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the MCP revision Eve requests. The server's answer to initialize is sent
// back in MCP-Protocol-Version on every later request.
const ProtocolVersion = "2025-06-18"

// Delivery errors. Callers test them with errors.Is; the wrapped message carries detail.
var (
	// ErrUnauthorized means T3 refused the token (expired, revoked, or missing scope), or no
	// usable token is stored. Retrying will not help until the user logs in again.
	ErrUnauthorized = errors.New("t3: unauthorized")
	// ErrUnavailable means T3 could not be reached or failed transiently; retry later.
	ErrUnavailable = errors.New("t3: unavailable")
	// ErrRejected means T3 understood the request and refused it (JSON-RPC error or a tool
	// result with isError). Retrying the same message will not help.
	ErrRejected = errors.New("t3: rejected")
)

// maxResponse bounds how much of a T3 response Eve reads.
const maxResponse = 4 << 20

// Client is a minimal MCP Streamable HTTP client for T3's /mcp endpoint, enough to call
// t3_thread_send. It keeps one MCP session and serializes calls; a Client must not be copied
// after first use.
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:3773
	Store   *TokenStore
	// HTTP is the transport; nil means a client with a 30 s timeout.
	HTTP *http.Client
	// Version is reported as clientInfo.version; empty means "dev".
	Version string

	mu        sync.Mutex
	ready     bool   // initialize and notifications/initialized done
	session   string // Mcp-Session-Id, if the server issued one
	protocol  string // negotiated protocol version
	sessToken string // access token the session was opened with
	nextID    int64
}

var defaultMCPHTTP = &http.Client{Timeout: 30 * time.Second}

// SendThread posts message into the T3 thread threadID with mode "auto": an idle thread starts
// a turn, a busy one queues it. clientRequestID makes redelivery idempotent on T3's side, so
// the outbox can retry freely.
func (c *Client) SendThread(ctx context.Context, threadID, message, clientRequestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	tok, err := c.Store.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	if !tok.ExpiresAt.IsZero() && !time.Now().Before(tok.ExpiresAt) {
		return fmt.Errorf("%w: token expired at %s; run `eve t3 login`", ErrUnauthorized, tok.ExpiresAt.Format(time.RFC3339))
	}
	if c.sessToken != tok.AccessToken {
		c.reset() // a new login: do not carry a session opened under the old token
	}

	params := map[string]any{
		"name": "t3_thread_send",
		"arguments": map[string]any{
			"threadId":        threadID,
			"message":         message,
			"mode":            "auto",
			"clientRequestId": clientRequestID,
		},
	}
	// One retry, for a session the server has dropped (e.g. after a T3 restart).
	for attempt := 0; ; attempt++ {
		if !c.ready {
			if err := c.initialize(ctx, tok.AccessToken); err != nil {
				c.reset()
				return err
			}
		}
		var res toolResult
		err := c.call(ctx, tok.AccessToken, "tools/call", params, &res)
		if errors.Is(err, errSessionGone) && attempt == 0 {
			c.reset()
			continue
		}
		if err != nil {
			c.reset() // cheap, and the next call starts from a known state
			return err
		}
		if res.IsError {
			return fmt.Errorf("%w: t3_thread_send: %s", ErrRejected, res.text())
		}
		return nil
	}
}

func (c *Client) reset() {
	c.ready, c.session, c.protocol, c.sessToken = false, "", "", ""
}

// initialize opens an MCP session: initialize, then notifications/initialized.
func (c *Client) initialize(ctx context.Context, token string) error {
	version := c.Version
	if version == "" {
		version = "dev"
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	err := c.call(ctx, token, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": ClientName, "version": version},
	}, &res)
	if errors.Is(err, errSessionGone) {
		// No session exists yet, so a 400/404 here is a plain refusal.
		return fmt.Errorf("%w: initialize: %w", ErrRejected, err)
	}
	if err != nil {
		return err
	}
	c.protocol = res.ProtocolVersion
	if c.protocol == "" {
		c.protocol = ProtocolVersion
	}
	if err := c.notify(ctx, token, "notifications/initialized"); err != nil {
		return err
	}
	c.ready, c.sessToken = true, token
	return nil
}

// rpcMessage is a JSON-RPC 2.0 message as Eve reads it.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// toolResult is the MCP CallToolResult subset Eve reads.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func (r toolResult) text() string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	if len(parts) == 0 {
		return "tool reported an error"
	}
	return strings.Join(parts, "; ")
}

// errSessionGone means the server no longer knows our Mcp-Session-Id (HTTP 404, or 400 while a
// session is in use): open a new session and try again.
var errSessionGone = errors.New("t3: MCP session expired")

// call sends a JSON-RPC request and decodes its result into out.
func (c *Client) call(ctx context.Context, token, method string, params, out any) error {
	c.nextID++
	id := c.nextID
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	resp, err := c.post(ctx, token, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := c.checkStatus(resp); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if method == "initialize" {
		c.session = resp.Header.Get("Mcp-Session-Id")
	}
	msg, err := readResponse(resp, id)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if msg.Error != nil {
		return fmt.Errorf("%w: %s: JSON-RPC %d: %s", ErrRejected, method, msg.Error.Code, msg.Error.Message)
	}
	if out != nil && len(msg.Result) > 0 {
		if err := json.Unmarshal(msg.Result, out); err != nil {
			return fmt.Errorf("%w: %s: decode result: %w", ErrRejected, method, err)
		}
	}
	return nil
}

// notify sends a JSON-RPC notification; the server answers 202 with no body.
func (c *Client) notify(ctx context.Context, token, method string) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	resp, err := c.post(ctx, token, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse)) // drain for connection reuse
	if err := c.checkStatus(resp); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

func (c *Client) post(ctx context.Context, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if c.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", c.protocol)
	} else {
		req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultMCPHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Connection refused, reset, timeout: T3 is down or restarting.
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return resp, nil
}

// checkStatus maps HTTP status codes onto the package's errors.
func (c *Client) checkStatus(resp *http.Response) error {
	code := resp.StatusCode
	switch {
	case code/100 == 2:
		return nil
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w: HTTP %d %s", ErrUnauthorized, code, authDetail(resp))
	case code == http.StatusNotFound && c.session != "":
		return fmt.Errorf("%w: HTTP 404", errSessionGone)
	case code == http.StatusBadRequest && c.session != "" && sessionComplaint(resp):
		return fmt.Errorf("%w: HTTP 400", errSessionGone)
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return fmt.Errorf("%w: HTTP %d", ErrUnavailable, code)
	}
	return fmt.Errorf("%w: HTTP %d %s", ErrRejected, code, bodySnippet(resp))
}

// authDetail returns the WWW-Authenticate error, which says why a token was refused.
func authDetail(resp *http.Response) string {
	return resp.Header.Get("WWW-Authenticate")
}

// sessionComplaint reports whether a 400 body is about the session header. Servers answer 400
// for a missing or unknown Mcp-Session-Id; anything else is a real bad request.
func sessionComplaint(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(bodySnippet(resp)), "session")
}

// bodySnippet reads up to 512 bytes of an error body. It consumes the body.
func bodySnippet(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return strings.TrimSpace(string(b))
}

// readResponse finds the JSON-RPC response with the given id in a JSON body or an SSE stream.
func readResponse(resp *http.Response, id int64) (*rpcMessage, error) {
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	body := io.LimitReader(resp.Body, maxResponse)
	if mt == "text/event-stream" {
		return readSSE(body, id)
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %w", ErrUnavailable, err)
	}
	// A JSON body holds one message or, in older revisions, a batch.
	msgs, err := decodeMessages(b)
	if err != nil {
		return nil, fmt.Errorf("%w: decode response: %w", ErrRejected, err)
	}
	for _, m := range msgs {
		if matchID(m.ID, id) {
			return m, nil
		}
	}
	return nil, fmt.Errorf("%w: no JSON-RPC response with id %d", ErrRejected, id)
}

// readSSE scans an event stream for the response to id, skipping server notifications and
// requests. A stream that ends first means the connection broke.
func readSSE(r io.Reader, id int64) (*rpcMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxResponse)
	var event string
	var data []string
	dispatch := func() (*rpcMessage, error) {
		defer func() { event, data = "", nil }()
		if len(data) == 0 || (event != "" && event != "message") {
			return nil, nil
		}
		msgs, err := decodeMessages([]byte(strings.Join(data, "\n")))
		if err != nil {
			return nil, fmt.Errorf("%w: decode SSE event: %w", ErrRejected, err)
		}
		for _, m := range msgs {
			if matchID(m.ID, id) && m.Method == "" {
				return m, nil
			}
		}
		return nil, nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if m, err := dispatch(); m != nil || err != nil {
				return m, err
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
		// Comments (":…"), id and retry lines are irrelevant for a single response.
	}
	if m, err := dispatch(); m != nil || err != nil {
		return m, err
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: read SSE stream: %w", ErrUnavailable, err)
	}
	return nil, fmt.Errorf("%w: SSE stream ended before the response to id %d", ErrUnavailable, id)
}

func decodeMessages(b []byte) ([]*rpcMessage, error) {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '[' {
		var batch []*rpcMessage
		err := json.Unmarshal(b, &batch)
		return batch, err
	}
	var m rpcMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return []*rpcMessage{&m}, nil
}

// matchID compares a raw JSON-RPC id with the numeric id Eve sent; servers may echo it as a
// number or, loosely, as a string.
func matchID(raw json.RawMessage, id int64) bool {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return s == fmt.Sprint(id)
}
