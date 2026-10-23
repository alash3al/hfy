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
	queue chan [2]int64 // conversation, event
	mu    sync.Mutex
	subs  map[int64]map[*Subscription]struct{}
}

// Subscription receives the events of one conversation. If the subscriber is too
// slow its channel is closed (Dropped); it reconnects with Last-Event-ID.
type Subscription struct {
	C       chan db.EventRow
	dropped bool
}

func NewHub(store *db.Store) *Hub {
	return &Hub{store: store, queue: make(chan [2]int64, 1024), subs: map[int64]map[*Subscription]struct{}{}}
}

func (h *Hub) Subscribe(conversation int64) (*Subscription, func()) {
	s := &Subscription{C: make(chan db.EventRow, 256)}
	h.mu.Lock()
	if h.subs[conversation] == nil {
		h.subs[conversation] = map[*Subscription]struct{}{}
	}
	h.subs[conversation][s] = struct{}{}
	h.mu.Unlock()
	return s, func() {
		h.mu.Lock()
		if _, ok := h.subs[conversation][s]; ok {
			delete(h.subs[conversation], s)
			if len(h.subs[conversation]) == 0 {
				delete(h.subs, conversation)
			}
			if !s.dropped {
				close(s.C)
			}
		}
		h.mu.Unlock()
	}
}

func (h *Hub) wanted(conversation int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[conversation]) > 0
}

// Notify queues the event a db.ChanEvents notification names for Run. It
// never reads the database: the listener calling it also carries stop
// requests and job wake-ups. It blocks only while Run is a full queue behind.
func (h *Hub) Notify(ctx context.Context, payload string) {
	cid, eid, ok := strings.Cut(payload, ":")
	if !ok {
		return
	}
	conversation, err1 := strconv.ParseInt(cid, 10, 64)
	event, err2 := strconv.ParseInt(eid, 10, 64)
	if err1 != nil || err2 != nil || !h.wanted(conversation) {
		return
	}
	select {
	case h.queue <- [2]int64{conversation, event}:
	case <-ctx.Done():
	}
}

// Run loads the queued events and pushes each to its conversation's
// subscribers until ctx ends. One consumer keeps every conversation in order.
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-h.queue:
			h.push(ctx, n[0], n[1])
		}
	}
}

func (h *Hub) push(ctx context.Context, conversation, event int64) {
	row, err := h.store.EventByID(ctx, h.store.Pool, event)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("hub: load event", "event", event, "err", err)
		}
		return
	}
	h.mu.Lock()
	for s := range h.subs[conversation] {
		select {
		case s.C <- row:
		default:
			s.dropped = true
			delete(h.subs[conversation], s)
			close(s.C)
		}
	}
	h.mu.Unlock()
}
