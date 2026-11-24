// Package hub is a tiny non-blocking publish/subscribe fan-out used for
// server-sent events. A slow subscriber is dropped rather than allowed to
// stall publishers; it reconnects and catches up from the database.
package hub

import "sync"

// Frame is one pre-encoded SSE message, shared (read-only) by all subscribers.
type Frame struct {
	V    int64  // store version this frame brings subscribers up to
	Data []byte // full SSE text: "id: ..\nevent: ..\ndata: ..\n\n"
}

type Sub struct {
	C       chan Frame
	dropped bool
}

// Dropped reports whether the hub closed this subscriber for being too slow
// (or because the hub was closed).
func (s *Sub) Dropped() bool { return s.dropped }

type Hub struct {
	mu     sync.Mutex
	subs   map[*Sub]struct{}
	closed bool
}

func New() *Hub { return &Hub{subs: make(map[*Sub]struct{})} }

// Subscribe registers a subscriber. It returns nil when max is reached or the
// hub is closed.
func (h *Hub) Subscribe(max int) *Sub {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || (max > 0 && len(h.subs) >= max) {
		return nil
	}
	s := &Sub{C: make(chan Frame, 32)}
	h.subs[s] = struct{}{}
	return s
}

func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.C)
	}
}

// Publish never blocks. Subscribers whose buffer is full are removed.
func (h *Hub) Publish(f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.C <- f:
		default:
			s.dropped = true
			delete(h.subs, s)
			close(s.C)
		}
	}
}

func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Close disconnects everyone and refuses new subscribers.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		s.dropped = true
		delete(h.subs, s)
		close(s.C)
	}
}
