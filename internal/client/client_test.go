package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
)

func sse(w http.ResponseWriter, ev api.Event) {
	b, _ := json.Marshal(ev)
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, b)
	w.(http.Flusher).Flush()
}

// The first connection drops after one event; the client must reconnect with Last-Event-ID
// and finish on the terminal event.
func TestWaitReconnectsWithLastEventID(t *testing.T) {
	var conns atomic.Int32
	var resumedFrom atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v1/jobs/j1/events":
			n := conns.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			if n == 1 {
				sse(w, api.Event{Seq: 1, JobID: "j1", Kind: api.EventTransition, State: api.StateArchiving})
				return // connection drops
			}
			id, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
			resumedFrom.Store(id)
			sse(w, api.Event{Seq: 2, JobID: "j1", Kind: api.EventTransition, State: api.StateSucceeded})
		case "/v1/jobs/j1":
			_ = json.NewEncoder(w).Encode(api.Result{ID: "j1", Outcome: api.StateSucceeded})
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	var seen []api.State
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := c.Wait(ctx, "j1", func(ev api.Event) { seen = append(seen, ev.State) })
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != api.StateSucceeded || len(seen) != 2 || resumedFrom.Load() != 1 {
		t.Fatalf("result %v seen %v resumed from %d", r.Outcome, seen, resumedFrom.Load())
	}
}

func TestWaitStopsOnAwaitingConfirmation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/jobs/j1/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			sse(w, api.Event{Seq: 5, Kind: api.EventTransition, State: api.StateAwaitingConfirmation})
			<-r.Context().Done() // the server keeps the stream open; the client must stop anyway
			return
		}
		_ = json.NewEncoder(w).Encode(api.Result{ID: "j1", Outcome: api.StateAwaitingConfirmation})
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := New(srv.URL, "tok").Wait(ctx, "j1", nil)
	if err != nil || api.ExitCode(r.Outcome) != api.ExitConfirmationRequired {
		t.Fatalf("%v %v", r.Outcome, err)
	}
}

func TestErrorsAreTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(api.ErrorBody{Error: "app \"x\" is not configured", Code: "unknown_app"})
	}))
	defer srv.Close()
	_, err := New(srv.URL, "tok").Submit(context.Background(), api.JobRequest{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "unknown_app" || ae.Status != 400 {
		t.Fatalf("%v", err)
	}
	_, err = New("http://127.0.0.1:1", "tok").Version(context.Background())
	var un *ErrUnreachable
	if !errors.As(err, &un) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}
