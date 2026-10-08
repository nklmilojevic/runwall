// Package hub fans out "something changed" signals to connected browsers.
package hub

import "sync"

type Hub struct {
	mu   sync.Mutex
	subs map[chan int64]struct{}
}

func New() *Hub {
	return &Hub{subs: make(map[chan int64]struct{})}
}

// Subscribe returns a channel of changed run IDs and a function to stop receiving them.
func (h *Hub) Subscribe() (<-chan int64, func()) {
	ch := make(chan int64, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Publish never blocks: a subscriber that is behind only needs to know that something changed.
func (h *Hub) Publish(runID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- runID:
		default:
		}
	}
}
