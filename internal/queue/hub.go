package queue

import (
	"sync"

	"github.com/self-deprecated/eve/internal/api"
)

// Hub fans committed job events out to live subscribers (SSE clients of `eve wait`). It holds
// no history: subscribers replay from the store first, then follow the hub.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan api.Event]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[string]map[chan api.Event]struct{}{}} }

// Subscribe returns a channel of the job's future events and a function to stop.
func (h *Hub) Subscribe(jobID string) (<-chan api.Event, func()) {
	ch := make(chan api.Event, 64)
	h.mu.Lock()
	if h.subs[jobID] == nil {
		h.subs[jobID] = map[chan api.Event]struct{}{}
	}
	h.subs[jobID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[jobID][ch]; ok {
			delete(h.subs[jobID], ch)
			close(ch)
			if len(h.subs[jobID]) == 0 {
				delete(h.subs, jobID)
			}
		}
		h.mu.Unlock()
	}
}

// Publish delivers ev to the job's subscribers. A subscriber that falls behind is dropped; its
// client reconnects with Last-Event-ID and replays from the store.
func (h *Hub) Publish(ev api.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[ev.JobID] {
		select {
		case ch <- ev:
		default:
			delete(h.subs[ev.JobID], ch)
			close(ch)
		}
	}
}
