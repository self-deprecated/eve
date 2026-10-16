package ledger

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
)

const quartz = "https://eve.tail.ts.net"

func TestIntentResolveCollect(t *testing.T) {
	l := Ledger{Path: filepath.Join(t.TempDir(), "jobs.json")}
	if p, err := l.Pending(); err != nil || len(p) != 0 {
		t.Fatal(p, err)
	}
	_ = l.Intend(Entry{Server: quartz, Key: "k1", App: "driver", Verb: api.VerbTestflight})
	_ = l.Intend(Entry{Server: quartz, Key: "k1", App: "other"}) // repeated key: ignored
	// The response was lost: the intent is pending without an id.
	if p, _ := l.Pending(); len(p) != 1 || p[0].ID != "" || p[0].App != "driver" {
		t.Fatalf("%+v", p)
	}
	_ = l.Resolve(quartz, "k1", "j1")
	_ = l.Collect(quartz, "j1", api.StateAwaitingConfirmation) // a pause is not an outcome
	if p, _ := l.Pending(); len(p) != 1 || p[0].ID != "j1" {
		t.Fatalf("%+v", p)
	}
	_ = l.Collect(quartz, "j1", api.StateSucceeded)
	if p, _ := l.Pending(); len(p) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestEntriesAreScopedByServer(t *testing.T) {
	l := Ledger{Path: filepath.Join(t.TempDir(), "jobs.json")}
	_ = l.Intend(Entry{Server: quartz, Key: "k1"})
	_ = l.Resolve(quartz, "k1", "j1")
	_ = l.Intend(Entry{Server: "http://127.0.0.1:4790", Key: "k2"})
	_ = l.Resolve("http://127.0.0.1:4790", "k2", "j1") // same id, other server
	_ = l.Collect("http://127.0.0.1:4790", "j1", api.StateFailed)
	p, _ := l.Pending()
	if len(p) != 1 || p[0].Server != quartz {
		t.Fatalf("collecting on one server touched another: %+v", p)
	}
	_ = l.Drop(quartz, "j1", "unknown to the server")
	if p, _ := l.Pending(); len(p) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestPruneKeepsUncollected(t *testing.T) {
	defer func(n int) { maxCollected = n }(maxCollected)
	maxCollected = 10
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	l := Ledger{Path: filepath.Join(t.TempDir(), "jobs.json"), Now: func() time.Time { return now }}
	_ = l.Intend(Entry{Server: quartz, Key: "old"})
	_ = l.Resolve(quartz, "old", "j-old")
	_ = l.Collect(quartz, "j-old", api.StateFailed)
	_ = l.Intend(Entry{Server: quartz, Key: "forgotten"}) // never collected
	now = now.Add(30 * 24 * time.Hour)
	for i := 0; i < maxCollected+20; i++ {
		k := fmt.Sprintf("k%d", i)
		_ = l.Intend(Entry{Server: quartz, Key: k})
		_ = l.Resolve(quartz, k, "j"+k)
		_ = l.Collect(quartz, "j"+k, api.StateSucceeded)
	}
	es, _ := l.read()
	var forgotten, old int
	for _, e := range es {
		if e.Key == "forgotten" {
			forgotten++
		}
		if e.Key == "old" {
			old++
		}
	}
	if forgotten != 1 || old != 0 || len(es) != maxCollected+1 {
		t.Fatalf("forgotten=%d old=%d len=%d", forgotten, old, len(es))
	}
}

func TestConcurrentWritesAreNotLost(t *testing.T) {
	l := Ledger{Path: filepath.Join(t.TempDir(), "jobs.json")}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = l.Intend(Entry{Server: quartz, Key: fmt.Sprint(i)})
		}(i)
	}
	wg.Wait()
	if p, _ := l.Pending(); len(p) != 20 {
		t.Fatalf("lost entries: %d", len(p))
	}
}
