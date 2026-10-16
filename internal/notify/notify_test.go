package notify

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/t3"
)

// fakeOutbox is an in-memory Outbox.
type fakeOutbox struct {
	mu   sync.Mutex
	rows []*fakeRow
}

type fakeRow struct {
	Delivery
	next      time.Time
	delivered bool
	exhausted bool
	lastErr   string
}

func (o *fakeOutbox) add(d Delivery) *fakeRow {
	o.mu.Lock()
	defer o.mu.Unlock()
	d.ID = int64(len(o.rows) + 1)
	r := &fakeRow{Delivery: d, next: d.CreatedAt}
	o.rows = append(o.rows, r)
	return r
}

func (o *fakeOutbox) get(id int64) fakeRow {
	o.mu.Lock()
	defer o.mu.Unlock()
	return *o.rows[id-1]
}

func (o *fakeOutbox) Due(_ context.Context, now time.Time, limit int) ([]Delivery, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []Delivery
	for _, r := range o.rows {
		if !r.delivered && !r.exhausted && !r.next.After(now) && len(out) < limit {
			out = append(out, r.Delivery)
		}
	}
	return out, nil
}

func (o *fakeOutbox) Delivered(_ context.Context, id int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rows[id-1].delivered = true
	return nil
}

func (o *fakeOutbox) Retry(_ context.Context, id int64, attempt int, next time.Time, lastErr string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.rows[id-1]
	if attempt != r.Attempts+1 {
		return fmt.Errorf("attempt %d after %d", attempt, r.Attempts)
	}
	r.Attempts, r.next, r.lastErr = attempt, next, lastErr
	return nil
}

func (o *fakeOutbox) Exhausted(_ context.Context, id int64, lastErr string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rows[id-1].exhausted, o.rows[id-1].lastErr = true, lastErr
	return nil
}

// senderFunc adapts a function to Sender.
type senderFunc func(ctx context.Context, threadID, message, clientRequestID string) error

func (f senderFunc) SendThread(ctx context.Context, threadID, message, clientRequestID string) error {
	return f(ctx, threadID, message, clientRequestID)
}

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

var t0 = time.Date(2026, 10, 16, 15, 12, 0, 0, time.UTC)

func notification(event string) api.Notification {
	return api.Notification{Event: event, ID: "j_1", DeliveryID: "d_7", Result: api.Result{
		ID: "j_1", App: "driver", Verb: api.VerbTestflight, Outcome: api.StateSucceeded,
	}}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{
		10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
		5 * time.Minute, 10 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute,
	}
	for i, w := range want {
		if got := Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestDeliverT3(t *testing.T) {
	ob := &fakeOutbox{}
	var got []string
	d := &Deliverer{Outbox: ob, Now: (&clock{t0}).Now, T3: senderFunc(func(_ context.Context, th, msg, crid string) error {
		got = append(got, th, crid, msg)
		return nil
	})}
	row := ob.add(Delivery{
		JobID: "j_1", Event: api.NotifySucceeded, Target: Target{KindT3, "thread-9"},
		Payload: notification(api.NotifySucceeded), CreatedAt: t0,
	})
	if err := d.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ob.get(row.ID).delivered {
		t.Fatal("not delivered")
	}
	if got[0] != "thread-9" || got[1] != "j_1:job.succeeded" || !strings.HasPrefix(got[2], "[eve] testflight driver j_1 → succeeded") {
		t.Errorf("sent %q", got)
	}
}

func TestRetryScheduleUntilExhausted(t *testing.T) {
	ob := &fakeOutbox{}
	clk := &clock{t0}
	var exhausted []Delivery
	var errs int
	d := &Deliverer{
		Outbox: ob, Now: clk.Now,
		T3: senderFunc(func(context.Context, string, string, string) error {
			return fmt.Errorf("%w: connection refused", t3.ErrUnavailable)
		}),
		OnExhausted: func(d Delivery) { exhausted = append(exhausted, d) },
		OnError:     func(Delivery, error) { errs++ },
	}
	row := ob.add(Delivery{
		JobID: "j_1", Event: api.NotifyFailed, Target: Target{KindT3, "th"},
		Payload: notification(api.NotifyFailed), CreatedAt: t0,
	})

	var gaps []time.Duration
	for i := 0; i < 1000 && !ob.get(row.ID).exhausted; i++ {
		before := clk.t
		if err := d.DeliverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		r := ob.get(row.ID)
		if r.exhausted {
			break
		}
		gaps = append(gaps, r.next.Sub(before))
		// Polling early must not attempt again.
		clk.t = r.next.Add(-time.Second)
		_ = d.DeliverOnce(context.Background())
		if ob.get(row.ID).Attempts != r.Attempts {
			t.Fatal("attempted before next")
		}
		clk.t = r.next
	}
	wantStart := []time.Duration{
		10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
		5 * time.Minute, 10 * time.Minute, 30 * time.Minute, 30 * time.Minute,
	}
	if !slices.Equal(gaps[:len(wantStart)], wantStart) {
		t.Errorf("schedule starts %v, want %v", gaps[:len(wantStart)], wantStart)
	}
	if !clk.t.Equal(t0.Add(MaxAge)) {
		t.Errorf("last attempt at %v, want exactly CreatedAt+24h", clk.t)
	}
	r := ob.get(row.ID)
	if !r.exhausted || !strings.Contains(r.lastErr, "connection refused") {
		t.Errorf("row %+v", r)
	}
	if len(exhausted) != 1 || exhausted[0].Attempts != len(gaps)+1 || exhausted[0].LastError == "" {
		t.Errorf("OnExhausted got %+v (gaps %d)", exhausted, len(gaps))
	}
	if errs != len(gaps)+1 {
		t.Errorf("OnError called %d times, want %d", errs, len(gaps)+1)
	}
}

func TestPermanentFailuresExhaustAtOnce(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad":
			http.Error(w, "nope", http.StatusBadRequest)
		case "/redirect":
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
		}
	}))
	defer hook.Close()
	for _, tc := range []struct {
		name   string
		target Target
		sender Sender
	}{
		{"t3 rejected", Target{KindT3, "th"}, senderFunc(func(context.Context, string, string, string) error {
			return fmt.Errorf("%w: thread not found", t3.ErrRejected)
		})},
		{"t3 not configured", Target{KindT3, "th"}, nil},
		{"webhook 400", Target{KindWebhook, hook.URL + "/bad"}, nil},
		{"webhook redirect", Target{KindWebhook, hook.URL + "/redirect"}, nil},
		{"unknown kind", Target{"pager", "x"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ob := &fakeOutbox{}
			var exhausted int
			d := &Deliverer{Outbox: ob, Now: (&clock{t0}).Now, T3: tc.sender, OnExhausted: func(Delivery) { exhausted++ }}
			row := ob.add(Delivery{
				JobID: "j_1", Event: api.NotifyFailed, Target: tc.target,
				Payload: notification(api.NotifyFailed), CreatedAt: t0,
			})
			if err := d.DeliverOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r := ob.get(row.ID); !r.exhausted || r.Attempts != 0 || exhausted != 1 {
				t.Errorf("row %+v, OnExhausted %d", r, exhausted)
			}
		})
	}
}

func TestUnauthorizedIsRetried(t *testing.T) {
	ob := &fakeOutbox{}
	var seen error
	d := &Deliverer{
		Outbox: ob, Now: (&clock{t0}).Now,
		T3: senderFunc(func(context.Context, string, string, string) error {
			return fmt.Errorf("%w: HTTP 401", t3.ErrUnauthorized)
		}),
		OnError: func(_ Delivery, err error) { seen = err },
	}
	row := ob.add(Delivery{JobID: "j_1", Event: api.NotifyFailed, Target: Target{KindT3, "th"}, CreatedAt: t0})
	_ = d.DeliverOnce(context.Background())
	if r := ob.get(row.ID); r.exhausted || r.Attempts != 1 || !r.next.Equal(t0.Add(10*time.Second)) {
		t.Errorf("row %+v", r)
	}
	if !errors.Is(seen, t3.ErrUnauthorized) {
		t.Errorf("OnError got %v", seen)
	}
}

// recordingHook is a webhook receiver that answers with a scripted status sequence.
type recordingHook struct {
	mu       sync.Mutex
	statuses []int
	reqs     []*http.Request
	bodies   [][]byte
}

func (h *recordingHook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	h.reqs, h.bodies = append(h.reqs, r), append(h.bodies, b)
	status := http.StatusOK
	if len(h.statuses) > 0 {
		status, h.statuses = h.statuses[0], h.statuses[1:]
	}
	w.WriteHeader(status)
}

func TestWebhookSignedAndRetried(t *testing.T) {
	rec := &recordingHook{statuses: []int{503, 429, 408, 204}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	ob := &fakeOutbox{}
	clk := &clock{t0}
	secrets := ConfigSecrets{
		Notify: config.Notify{Secrets: []config.HookToken{
			{Prefix: srv.URL + "/", Secret: "file:short"},
			{Prefix: srv.URL + "/hooks/", Secret: "file:long"},
		}},
		Resolve: func(ref string) (string, error) { return "secret-" + strings.TrimPrefix(ref, "file:"), nil },
	}
	d := &Deliverer{Outbox: ob, Now: clk.Now, Secrets: secrets, Version: "1.2.3"}
	row := ob.add(Delivery{
		JobID: "j_1", Event: api.NotifySucceeded, Target: Target{KindWebhook, srv.URL + "/hooks/eve"},
		Payload: notification(api.NotifySucceeded), CreatedAt: t0,
	})

	for range 4 {
		if err := d.DeliverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		clk.t = ob.get(row.ID).next
	}
	r := ob.get(row.ID)
	if !r.delivered || r.Attempts != 3 {
		t.Fatalf("row %+v", r)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reqs) != 4 {
		t.Fatalf("%d requests", len(rec.reqs))
	}
	for i, req := range rec.reqs {
		if got := req.Header.Get("X-Eve-Delivery"); got != "d_7" {
			t.Errorf("attempt %d X-Eve-Delivery %q, want stable d_7", i, got)
		}
		if req.Header.Get("X-Eve-Event") != api.NotifySucceeded || req.Header.Get("User-Agent") != "eve/1.2.3" ||
			req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers %v", req.Header)
		}
		want := Sign("secret-long", rec.bodies[i]) // longest prefix wins
		if !hmac.Equal([]byte(req.Header.Get("X-Eve-Signature")), []byte(want)) {
			t.Errorf("signature %q, want %q", req.Header.Get("X-Eve-Signature"), want)
		}
	}
	var n api.Notification
	if err := json.Unmarshal(rec.bodies[0], &n); err != nil || n.Event != api.NotifySucceeded || n.Result.ID != "j_1" {
		t.Errorf("body %s: %v", rec.bodies[0], err)
	}
}

func TestSign(t *testing.T) {
	// RFC 4231 test case 2.
	got := Sign("Jefe", []byte("what do ya want for nothing?"))
	if want := "sha256=5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"; got != want {
		t.Errorf("got %s", got)
	}
}

func TestHumanHookBody(t *testing.T) {
	rec := &recordingHook{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	ob := &fakeOutbox{}
	human := &config.HumanHook{URL: srv.URL + "/grok", Secret: "file:x"}
	d := &Deliverer{Outbox: ob, Now: (&clock{t0}).Now, Secrets: ConfigSecrets{
		Notify:  config.Notify{Human: human},
		Resolve: func(string) (string, error) { return "hs", nil },
	}}
	n := notification(api.NotifyNeedsHuman)
	n.Result.Verb, n.Result.App = api.VerbDevice, "lineage"
	n.Result.Human = &api.Human{Action: api.HumanUnlockPhone, Detail: "iPhone 16 is locked"}
	ob.add(Delivery{JobID: "j_1", Event: api.NotifyNeedsHuman, Target: Target{KindHuman, human.URL}, Payload: n, CreatedAt: t0})
	if err := d.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var body HumanMessage
	if err := json.Unmarshal(rec.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	want := "eve: lineage device j_1 needs you: unlock the iPhone — iPhone 16 is locked"
	if body.Text != want || body.Event != api.NotifyNeedsHuman || body.Payload.Result.Human.Action != api.HumanUnlockPhone {
		t.Errorf("body %+v", body)
	}
	if rec.reqs[0].Header.Get("X-Eve-Signature") != Sign("hs", rec.bodies[0]) {
		t.Error("human hook not signed")
	}
}

func TestHumanTextExhausted(t *testing.T) {
	got := HumanText(EventDeliveryExhausted, notification(api.NotifySucceeded))
	want := "eve: driver testflight j_1: could not deliver job.succeeded to the requesting agent within 24 h; see `eve status j_1`"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestKickDeliversPromptly(t *testing.T) {
	ob := &fakeOutbox{}
	delivered := make(chan struct{}, 1)
	d := &Deliverer{Outbox: ob, Interval: time.Hour, T3: senderFunc(func(context.Context, string, string, string) error {
		delivered <- struct{}{}
		return nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error)
	go func() { done <- d.Run(ctx) }()

	ob.add(Delivery{JobID: "j_1", Event: api.NotifySucceeded, Target: Target{KindT3, "th"}, CreatedAt: time.Now().Add(-time.Second)})
	d.Kick()
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("Kick did not trigger delivery")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v", err)
	}
}

func TestCancelledAttemptIsNotRecorded(t *testing.T) {
	ob := &fakeOutbox{}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Deliverer{Outbox: ob, Now: (&clock{t0}).Now, T3: senderFunc(func(ctx context.Context, _, _, _ string) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})}
	row := ob.add(Delivery{JobID: "j_1", Event: api.NotifySucceeded, Target: Target{KindT3, "th"}, CreatedAt: t0})
	_ = d.DeliverOnce(ctx)
	if r := ob.get(row.ID); r.Attempts != 0 || r.exhausted || r.delivered {
		t.Errorf("row %+v", r)
	}
}

func TestDeliveryTimeAllowList(t *testing.T) {
	ob := &fakeOutbox{}
	d := &Deliverer{Outbox: ob, Now: (&clock{t0}).Now, AllowHosts: []string{"hooks.example"}}
	row := ob.add(Delivery{JobID: "j_1", Event: api.NotifySucceeded, Target: Target{KindWebhook, "https://removed.example/x"}, CreatedAt: t0})
	_ = d.DeliverOnce(context.Background())
	if r := ob.get(row.ID); !r.exhausted || !strings.Contains(r.lastErr, "allowHosts") {
		t.Errorf("row %+v", r)
	}
}
