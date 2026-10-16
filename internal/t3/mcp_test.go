package t3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMCP is a Streamable HTTP MCP server that enforces the session lifecycle: initialize
// (which issues a session id), then notifications/initialized on that session, then tools/call.
type fakeMCP struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	sse         bool                // answer requests as event streams
	sessions    map[string]bool     // session id → initialized
	inits       int                 // initialize requests seen
	calls       []map[string]any    // tools/call arguments
	status      int                 // force this HTTP status on tools/call
	toolError   string              // answer tools/call with isError and this text
	rpcError    *rpcError           // answer tools/call with this JSON-RPC error
	reply       func(id any) string // custom tools/call body, overrides the above
	dropSession bool                // forget all sessions before the next tools/call
}

func newFakeMCP(t *testing.T) *fakeMCP {
	f := &fakeMCP{t: t, sessions: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMCP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/mcp" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") || !strings.Contains(r.Header.Get("Accept"), "application/json") {
		f.t.Errorf("Accept = %q", r.Header.Get("Accept"))
	}
	if r.Header.Get("MCP-Protocol-Version") != ProtocolVersion {
		f.t.Errorf("MCP-Protocol-Version = %q", r.Header.Get("MCP-Protocol-Version"))
	}
	var msg struct {
		ID     any             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")

	switch msg.Method {
	case "initialize":
		if sid != "" {
			http.Error(w, "initialize must not carry a session", 400)
			return
		}
		f.inits++
		sid = fmt.Sprintf("s%d", f.inits)
		f.sessions[sid] = false
		w.Header().Set("Mcp-Session-Id", sid)
		f.respond(w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":{"protocolVersion":%q,"capabilities":{"tools":{}},"serverInfo":{"name":"t3"}}}`, jsonID(msg.ID), ProtocolVersion))
	case "notifications/initialized":
		if _, ok := f.sessions[sid]; !ok {
			http.Error(w, "unknown session", 404)
			return
		}
		f.sessions[sid] = true
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		if f.dropSession {
			f.sessions, f.dropSession = map[string]bool{}, false
		}
		ready, ok := f.sessions[sid]
		if !ok {
			http.Error(w, "session not found", 404)
			return
		}
		if !ready {
			f.t.Error("tools/call before notifications/initialized")
			http.Error(w, "not initialized", 400)
			return
		}
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.Name != "t3_thread_send" {
			f.t.Errorf("tool %q", p.Name)
		}
		f.calls = append(f.calls, p.Arguments)
		switch {
		case f.status != 0:
			http.Error(w, "forced", f.status)
		case f.reply != nil:
			f.respond(w, f.reply(msg.ID))
		case f.rpcError != nil:
			b, _ := json.Marshal(f.rpcError)
			f.respond(w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"error":%s}`, jsonID(msg.ID), b))
		case f.toolError != "":
			f.respond(w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":{"content":[{"type":"text","text":%q}],"isError":true}}`, jsonID(msg.ID), f.toolError))
		default:
			f.respond(w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":{"content":[{"type":"text","text":"{\"queued\":true}"}]}}`, jsonID(msg.ID)))
		}
	default:
		http.Error(w, "unexpected method "+msg.Method, 400)
	}
}

func jsonID(id any) string {
	b, _ := json.Marshal(id)
	return string(b)
}

// respond writes one JSON-RPC message, as JSON or as an SSE stream with some noise around it.
func (f *fakeMCP) respond(w http.ResponseWriter, body string) {
	if !f.sse {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprint(w, ": keepalive\n\n")
	_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
	_, _ = fmt.Fprintf(w, "id: 1\nevent: message\ndata: %s\n\n", body)
}

func newClient(t *testing.T, base string) *Client {
	store := newStore(t)
	if err := store.SaveToken(&Token{AccessToken: "tok", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return &Client{BaseURL: base, Store: store}
}

func TestSendThread(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			f := newFakeMCP(t)
			f.sse = sse
			c := newClient(t, f.srv.URL)
			ctx := context.Background()
			if err := c.SendThread(ctx, "thread-1", "hello", "j_1:job.succeeded"); err != nil {
				t.Fatal(err)
			}
			if err := c.SendThread(ctx, "thread-2", "again", "j_2:job.failed"); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.inits != 1 {
				t.Errorf("initialize sent %d times, want 1 (session reused)", f.inits)
			}
			want := map[string]any{"threadId": "thread-1", "message": "hello", "mode": "auto", "clientRequestId": "j_1:job.succeeded"}
			if len(f.calls) != 2 || fmt.Sprint(f.calls[0]) != fmt.Sprint(want) {
				t.Errorf("calls = %v", f.calls)
			}
		})
	}
}

func TestSendThreadReinitializesExpiredSession(t *testing.T) {
	f := newFakeMCP(t)
	c := newClient(t, f.srv.URL)
	ctx := context.Background()
	if err := c.SendThread(ctx, "th", "one", "a"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.dropSession = true // as after a T3 restart
	f.mu.Unlock()
	if err := c.SendThread(ctx, "th", "two", "b"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inits != 2 {
		t.Errorf("initialize sent %d times, want 2", f.inits)
	}
}

func TestSendThreadErrors(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(f *fakeMCP, c *Client)
		want  error
		text  string
	}{
		{"unauthorized", func(f *fakeMCP, c *Client) {
			_ = c.Store.SaveToken(&Token{AccessToken: "revoked", TokenType: "Bearer"})
		}, ErrUnauthorized, "invalid_token"},
		{"expired locally", func(f *fakeMCP, c *Client) {
			_ = c.Store.SaveToken(&Token{AccessToken: "tok", ExpiresAt: time.Now().Add(-time.Minute)})
		}, ErrUnauthorized, "expired"},
		{"no token", func(f *fakeMCP, c *Client) {
			c.Store = newStore(t)
		}, ErrUnauthorized, "login"},
		{"server error", func(f *fakeMCP, c *Client) { f.status = 503 }, ErrUnavailable, "503"},
		{"tool error", func(f *fakeMCP, c *Client) { f.toolError = "thread not found" }, ErrRejected, "thread not found"},
		{"rpc error", func(f *fakeMCP, c *Client) {
			f.rpcError = &rpcError{Code: -32602, Message: "invalid params"}
		}, ErrRejected, "invalid params"},
		{"forbidden", func(f *fakeMCP, c *Client) { f.status = 403 }, ErrUnauthorized, "403"},
		{"truncated stream", func(f *fakeMCP, c *Client) {
			f.sse = true
			f.reply = func(any) string { return `{"jsonrpc":"2.0","method":"notifications/message","params":{}}` }
		}, ErrUnavailable, "ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMCP(t)
			c := newClient(t, f.srv.URL)
			f.mu.Lock()
			tc.setup(f, c)
			f.mu.Unlock()
			err := c.SendThread(ctx, "th", "m", "r")
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err = %v, want %v containing %q", err, tc.want, tc.text)
			}
		})
	}
}

func TestSendThreadConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	err := newClient(t, base).SendThread(context.Background(), "th", "m", "r")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestSendThreadNewLoginDropsSession(t *testing.T) {
	f := newFakeMCP(t)
	c := newClient(t, f.srv.URL)
	ctx := context.Background()
	if err := c.SendThread(ctx, "th", "one", "a"); err != nil {
		t.Fatal(err)
	}
	// The user logs in again; T3 issues the same bearer in this fake, but a different stored
	// token value must not reuse a session opened under the old one.
	c.sessToken = "older"
	if err := c.SendThread(ctx, "th", "two", "b"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inits != 2 {
		t.Errorf("initialize sent %d times, want 2", f.inits)
	}
}
