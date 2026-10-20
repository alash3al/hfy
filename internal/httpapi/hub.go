package httpapi

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/alash3al/hfy/internal/db"
)

// Hub fans events out to the SSE subscribers connected to this instance.
// Appending an event NOTIFYs; every instance receives it and pushes it to its
// own subscribers, so it works across instances. The table is read only to
// load the event for local subscribers and for replays.
type Hub struct {
	store *db.Store
	mu    sync.Mutex
	subs  map[int64]map[*Subscription]struct{}
}

// Subscription receives the events of one session. If the subscriber is too
// slow its channel is closed (Dropped); it reconnects with Last-Event-ID.
type Subscription struct {
	C       chan db.EventRow
	dropped bool
}

func NewHub(store *db.Store) *Hub {
	return &Hub{store: store, subs: map[int64]map[*Subscription]struct{}{}}
}

func (h *Hub) Subscribe(session int64) (*Subscription, func()) {
	s := &Subscription{C: make(chan db.EventRow, 256)}
	h.mu.Lock()
	if h.subs[session] == nil {
		h.subs[session] = map[*Subscription]struct{}{}
	}
	h.subs[session][s] = struct{}{}
	h.mu.Unlock()
	return s, func() {
		h.mu.Lock()
		if _, ok := h.subs[session][s]; ok {
			delete(h.subs[session], s)
			if len(h.subs[session]) == 0 {
				delete(h.subs, session)
			}
			if !s.dropped {
				close(s.C)
			}
		}
		h.mu.Unlock()
	}
}

func (h *Hub) wanted(session int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[session]) > 0
}

// Run listens for event notifications until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	h.store.Listen(ctx, func(_, payload string) {
		sid, eid, ok := strings.Cut(payload, ":")
		if !ok {
			return
		}
		session, err1 := strconv.ParseInt(sid, 10, 64)
		event, err2 := strconv.ParseInt(eid, 10, 64)
		if err1 != nil || err2 != nil || !h.wanted(session) {
			return
		}
		row, err := h.store.EventByID(ctx, h.store.Pool, event)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("hub: load event", "event", event, "err", err)
			}
			return
		}
		h.mu.Lock()
		for s := range h.subs[session] {
			select {
			case s.C <- row:
			default:
				s.dropped = true
				delete(h.subs[session], s)
				close(s.C)
			}
		}
		h.mu.Unlock()
	}, nil, db.ChanEvents)
}
