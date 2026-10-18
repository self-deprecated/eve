// Package ledger remembers, on the machine that ran `eve submit`, which jobs were submitted
// and whether anyone has seen their outcome. If the process waiting on a job dies (a harness
// restart, a session that ended), `eve pending` finds the jobs nobody collected (PLAN.md §8).
//
// An entry is created before the job is submitted (an intent, identified by its idempotency
// key), resolved to a job id once the server accepts it, and collected once a terminal outcome
// has been shown to someone. Entries are scoped by server: a job id means nothing elsewhere.
package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/flock"
)

// Entry is one submission.
type Entry struct {
	Server      string    `json:"server"`
	Key         string    `json:"key"`          // idempotency key; known before the server answers
	ID          string    `json:"id,omitempty"` // empty until the server accepted the job
	App         string    `json:"app"`
	Verb        api.Verb  `json:"verb"`
	Dir         string    `json:"dir,omitempty"` // where it was submitted from
	Submitted   time.Time `json:"submitted"`
	Collected   bool      `json:"collected,omitempty"`
	CollectedAt time.Time `json:"collectedAt,omitzero"`
	Outcome     api.State `json:"outcome,omitempty"`
	Note        string    `json:"note,omitempty"` // why it was dropped, when it was
}

// Ledger is a JSON file guarded by an advisory lock.
type Ledger struct {
	Path string
	Now  func() time.Time
}

// Collected entries are dropped after a week; past maxCollected, the oldest collected go first.
// Uncollected entries are never evicted: they are the safety net.
const keepCollected = 7 * 24 * time.Hour

var maxCollected = 500 // a variable so tests can lower it

// Default is $EVE_STATE_DIR/jobs.json, else $XDG_STATE_HOME/eve/jobs.json, else
// ~/.local/state/eve/jobs.json.
func Default() Ledger {
	dir := os.Getenv("EVE_STATE_DIR")
	if dir == "" {
		if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			dir = filepath.Join(x, "eve")
		} else {
			dir = config.ExpandHome("~/.local/state/eve")
		}
	}
	return Ledger{Path: filepath.Join(dir, "jobs.json")}
}

func (l Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Intend records a submission before it is sent. A repeated (server, key) changes nothing.
func (l Ledger) Intend(e Entry) error {
	return l.update(func(es []Entry) []Entry {
		for _, x := range es {
			if x.Server == e.Server && x.Key == e.Key {
				return es
			}
		}
		if e.Submitted.IsZero() {
			e.Submitted = l.now()
		}
		e.ID, e.Collected = "", false
		return append(es, e)
	})
}

// Resolve attaches the job id the server returned for (server, key).
func (l Ledger) Resolve(server, key, id string) error {
	return l.update(func(es []Entry) []Entry {
		for i := range es {
			if es[i].Server == server && es[i].Key == key {
				es[i].ID = id
				return es
			}
		}
		return append(es, Entry{Server: server, Key: key, ID: id, Submitted: l.now()})
	})
}

// Collect marks a job's outcome as seen. Only terminal outcomes collect a job: a job awaiting
// confirmation still needs its agent. Call it after the outcome was shown, never before.
func (l Ledger) Collect(server, id string, outcome api.State) error {
	if !outcome.Terminal() || id == "" {
		return nil
	}
	return l.mark(server, id, func(e *Entry) {
		e.Collected, e.CollectedAt, e.Outcome = true, l.now(), outcome
	})
}

// Drop stops tracking a job the server no longer knows, recording why.
func (l Ledger) Drop(server, id, note string) error {
	return l.mark(server, id, func(e *Entry) {
		e.Collected, e.CollectedAt, e.Note = true, l.now(), note
	})
}

func (l Ledger) mark(server, id string, fn func(*Entry)) error {
	return l.update(func(es []Entry) []Entry {
		for i := range es {
			if es[i].Server == server && es[i].ID == id && !es[i].Collected {
				fn(&es[i])
			}
		}
		return es
	})
}

// Pending returns the submissions nobody has collected, oldest first.
func (l Ledger) Pending() ([]Entry, error) {
	es, err := l.read()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range es {
		if !e.Collected {
			out = append(out, e)
		}
	}
	return out, nil
}

func (l Ledger) read() ([]Entry, error) {
	b, err := os.ReadFile(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var es []Entry
	if len(b) > 0 {
		if err := json.Unmarshal(b, &es); err != nil {
			return nil, fmt.Errorf("%s: %w", l.Path, err)
		}
	}
	return es, nil
}

// update applies fn under an exclusive lock and replaces the file atomically and durably.
func (l Ledger) update(fn func([]Entry) []Entry) error {
	dir := filepath.Dir(l.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(l.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := flock.Lock(lock, true); err != nil {
		return err
	}
	defer func() { _ = flock.Unlock(lock) }()

	es, err := l.read()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(l.prune(fn(es)), "", "  ")
	if err != nil {
		return err
	}
	tmp := l.Path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.Path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (l Ledger) prune(es []Entry) []Entry {
	cutoff := l.now().Add(-keepCollected)
	var open, collected []Entry
	for _, e := range es {
		switch {
		case !e.Collected:
			open = append(open, e)
		case e.CollectedAt.After(cutoff):
			collected = append(collected, e)
		}
	}
	sort.SliceStable(collected, func(i, j int) bool { return collected[i].CollectedAt.Before(collected[j].CollectedAt) })
	if len(collected) > maxCollected {
		collected = collected[len(collected)-maxCollected:]
	}
	out := append(open, collected...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Submitted.Before(out[j].Submitted) })
	return out
}
