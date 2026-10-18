// Package notify delivers job notifications from Eve's durable outbox (PLAN.md §8) to generic
// webhooks and an optional human webhook. Agents are not notified here: they follow their jobs
// with `eve wait`, which streams events from the server.
//
// The store writes one outbox row per (event, target) in the same transaction as the job
// transition, then calls Deliverer.Kick. The Deliverer sends due rows, records success, and
// reschedules failures with backoff (Backoff) for MaxAge after the row was created; after that,
// or at once for a permanent refusal, the row is exhausted and OnExhausted lets the server copy
// a one-liner to the human hook (ExhaustedCopy).
//
// # Webhooks
//
// A generic webhook (`eve submit --notify-url`) receives a POST whose body is the
// api.Notification JSON ({event, id, deliveryId, result}) with these headers:
//
//	Content-Type:    application/json
//	User-Agent:      eve/<version>
//	X-Eve-Event:     job.succeeded (the event name)
//	X-Eve-Delivery:  the notification's deliveryId, stable across retries; dedupe on it
//	X-Eve-Signature: sha256=<hex HMAC-SHA256 of the raw body>, when a secret applies
//
// Any 2xx is success. 408, 429, 5xx and network errors are retried; redirects and other 4xx are
// permanent. Each attempt times out after 10 s.
//
// # Human webhook
//
// notify.human.url (for example a chat bot or push service) receives the same headers and a body made
// for chat bots:
//
//	{
//	  "text":    "eve: lineage device j_01… needs you: unlock the iPhone — device is locked",
//	  "event":   "job.needs_human",
//	  "payload": { …the api.Notification… }
//	}
//
// text is one plain-text line. event is job.needs_human, job.waiting_for_device, or
// delivery.exhausted when an agent could not be reached for MaxAge (payload is then the
// notification that was not delivered). Size-guard confirmations never go to the human: they
// are the agent's call.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/self-deprecated/eve/internal/api"
)

// Target kinds.
const (
	KindWebhook = "webhook"
	KindHuman   = "human"
)

// EventDeliveryExhausted is the event of a human-hook copy of a delivery that gave up.
const EventDeliveryExhausted = "delivery.exhausted"

// MaxAge is how long after its creation a delivery keeps being retried.
const MaxAge = 24 * time.Hour

// Target is one recipient of a notification.
type Target struct {
	Kind    string // KindWebhook or KindHuman
	Address string // webhook URL
}

// Delivery is one outbox row: a notification for one target.
type Delivery struct {
	ID        int64
	JobID     string
	Event     string
	Target    Target
	Payload   api.Notification
	Attempts  int // attempts made before this one
	CreatedAt time.Time
	// LastError is the latest failure; the Deliverer sets it before calling OnExhausted.
	LastError string
}

// Outbox is the durable queue the store implements.
type Outbox interface {
	// Due returns up to limit undelivered, unexhausted rows whose next attempt is at or before
	// now, oldest first.
	Due(ctx context.Context, now time.Time, limit int) ([]Delivery, error)
	// Delivered marks a row sent.
	Delivered(ctx context.Context, id int64) error
	// Retry records a failed attempt (attempt counts it) and when to try again.
	Retry(ctx context.Context, id int64, attempt int, next time.Time, lastErr string) error
	// Exhausted gives up on a row; it stays visible to `eve status`.
	Exhausted(ctx context.Context, id int64, lastErr string) error
}

// ErrPermanent marks a failure that retrying cannot fix. Deliveries failing with it are
// exhausted at once.
var ErrPermanent = errors.New("permanent delivery failure")

// IsPermanent reports whether err should end a delivery without retrying.
func IsPermanent(err error) bool {
	return errors.Is(err, ErrPermanent)
}

// backoff is the retry schedule; the last step repeats until MaxAge.
var backoff = []time.Duration{
	10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
	5 * time.Minute, 10 * time.Minute, 30 * time.Minute,
}

// Backoff returns the delay after the attempt-th failed attempt (1-based).
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return backoff[min(attempt, len(backoff))-1]
}

// Deliverer drains an Outbox. Its zero value is not usable: set Outbox, and HTTP and Secrets
// as needed.
type Deliverer struct {
	Outbox  Outbox
	HTTP    *http.Client // nil: a plain client; redirects are never followed
	Secrets Secrets      // nil: webhooks are unsigned
	// AllowHosts, when set, re-checks webhook URLs at delivery time (see AllowedURL), so a
	// host removed from server.yml stops receiving queued notifications.
	AllowHosts []string
	Now        func() time.Time // nil: time.Now
	// OnExhausted is called after a delivery is given up; the server uses it to copy a
	// one-liner to the human hook (ExhaustedCopy).
	OnExhausted func(Delivery)
	// OnError is called after every failed attempt (logging).
	OnError   func(Delivery, error)
	Version   string        // for User-Agent; empty means "dev"
	Interval  time.Duration // poll interval; 0 means 5 s
	BatchSize int           // rows per Due call; 0 means 50
	Logger    *slog.Logger  // nil: slog.Default()

	once sync.Once
	kick chan struct{}
	mu   sync.Mutex // one DeliverOnce at a time
}

// WebhookTimeout bounds each webhook attempt.
const WebhookTimeout = 10 * time.Second

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Deliverer) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

func (d *Deliverer) kickChan() chan struct{} {
	d.once.Do(func() { d.kick = make(chan struct{}, 1) })
	return d.kick
}

// Kick asks Run to deliver now instead of at the next poll. Call it after committing outbox
// rows. It never blocks.
func (d *Deliverer) Kick() {
	select {
	case d.kickChan() <- struct{}{}:
	default: // a kick is already pending
	}
}

// Run delivers due rows every Interval and on Kick until ctx is done.
func (d *Deliverer) Run(ctx context.Context) error {
	interval := d.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	kick := d.kickChan()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := d.DeliverOnce(ctx); err != nil && ctx.Err() == nil {
			d.logger().Error("notify: outbox", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-kick:
		}
	}
}

// maxBatches bounds one DeliverOnce, so an outbox that fails to record progress cannot spin.
const maxBatches = 10

// DeliverOnce sends every due row once. Delivery failures are recorded in the outbox, not
// returned; the error reports outbox failures only.
func (d *Deliverer) DeliverOnce(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	limit := d.BatchSize
	if limit <= 0 {
		limit = 50
	}
	var errs []error
	for range maxBatches {
		due, err := d.Outbox.Due(ctx, d.now(), limit)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, row := range due {
			if ctx.Err() != nil {
				return errors.Join(append(errs, ctx.Err())...)
			}
			if err := d.handle(ctx, row); err != nil {
				errs = append(errs, err)
			}
		}
		if len(due) < limit {
			break
		}
	}
	return errors.Join(errs...)
}

// handle attempts one row and records the outcome.
func (d *Deliverer) handle(ctx context.Context, row Delivery) error {
	err := d.deliver(ctx, row)
	if err == nil {
		return d.Outbox.Delivered(ctx, row.ID)
	}
	if ctx.Err() != nil {
		return nil // shutting down: the row stays due and is retried on start
	}
	attempt := row.Attempts + 1
	msg := truncate(err.Error(), 500)
	d.logger().Warn("notify: delivery failed", "id", row.ID, "job", row.JobID, "event", row.Event,
		"kind", row.Target.Kind, "attempt", attempt, "err", msg)
	if d.OnError != nil {
		d.OnError(row, err)
	}

	now := d.now()
	deadline := row.CreatedAt.Add(MaxAge)
	if IsPermanent(err) || !now.Before(deadline) {
		if xerr := d.Outbox.Exhausted(ctx, row.ID, msg); xerr != nil {
			return xerr
		}
		row.Attempts, row.LastError = attempt, msg
		if d.OnExhausted != nil {
			d.OnExhausted(row)
		}
		return nil
	}
	// The final attempt lands exactly on the deadline; if it fails too, the row is exhausted.
	next := now.Add(Backoff(attempt))
	if next.After(deadline) {
		next = deadline
	}
	return d.Outbox.Retry(ctx, row.ID, attempt, next, msg)
}

// deliver performs one attempt.
func (d *Deliverer) deliver(ctx context.Context, row Delivery) error {
	switch row.Target.Kind {
	case KindWebhook:
		if d.AllowHosts != nil {
			if err := AllowedURL(row.Target.Address, d.AllowHosts); err != nil {
				return fmt.Errorf("%w: %w", ErrPermanent, err)
			}
		}
		return d.postWebhook(ctx, row, row.Payload)
	case KindHuman:
		return d.postWebhook(ctx, row, HumanBody(row))
	}
	return fmt.Errorf("%w: unknown target kind %q", ErrPermanent, row.Target.Kind)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
