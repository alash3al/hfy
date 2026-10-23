package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"uuid"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/metrics"
)

const (
	pingEvery    = 15 * time.Second
	catchupEvery = 5 * time.Second // safety net for a missed NOTIFY
	writeWindow  = 30 * time.Second
	replayPage   = 500
)

// grantCheckEvery is how soon an open stream notices its grant was revoked
// (one indexed lookup per stream per tick); expiry ends it on time.
var grantCheckEvery = 15 * time.Second

// streamEvents is the live SSE stream of a conversation, across turns and actors,
// durable and ephemeral events alike. Resume with Last-Event-ID, or ?after=
// on a first connection; with neither it starts from now. It ends when the
// grant does: the client reconnects with a fresh token.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, g *db.GrantRow) error {
	ctx := r.Context()

	after, err := s.resumePoint(r, g.ConversationID)
	if err != nil {
		return err
	}
	// Subscribe before replaying so nothing falls between the two.
	sub, cancel := s.hub.Subscribe(g.ConversationID)
	defer cancel()
	metrics.Streams.Inc()
	defer metrics.Streams.Dec()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(row db.EventRow) error {
		// Long-lived stream: push the write deadline out instead of disabling timeouts.
		_ = rc.SetWriteDeadline(time.Now().Add(writeWindow))
		data, err := json.Marshal(row.Envelope())
		if err != nil {
			return nil // skip an event we cannot encode
		}
		if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", row.UUID, row.Type, data); err != nil {
			return err
		}
		return rc.Flush()
	}
	replay := func() error {
		for {
			rows, err := s.store.EventsAfter(ctx, g.ConversationID, after, replayPage)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if err := send(r); err != nil {
					return err
				}
				after = r.ID
			}
			if len(rows) < replayPage {
				return nil
			}
		}
	}
	if err := rc.Flush(); err != nil {
		return nil
	}
	if err := replay(); err != nil {
		return nil
	}

	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	catchup := time.NewTicker(catchupEvery)
	defer catchup.Stop()
	expired := time.NewTimer(time.Until(g.ExpiresAt))
	defer expired.Stop()
	recheck := time.NewTicker(grantCheckEvery)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-expired.C:
			return nil
		case <-recheck.C:
			if live, err := s.store.GrantLive(ctx, g.ID); err != nil || !live {
				return nil // revoked, or unsure: fail closed, the client reconnects
			}
		case row, ok := <-sub.C:
			if !ok {
				return nil // too slow or shutting down: the client reconnects with Last-Event-ID
			}
			if row.ID <= after {
				continue
			}
			if err := send(row); err != nil {
				return nil
			}
			after = row.ID
		case <-catchup.C:
			if err := replay(); err != nil {
				return nil
			}
		case <-ping.C:
			_ = rc.SetWriteDeadline(time.Now().Add(writeWindow))
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return nil
			}
			if err := rc.Flush(); err != nil {
				return nil
			}
		}
	}
}

// resumePoint maps Last-Event-ID / ?after= (event uuids) to an internal id;
// with neither it is the current end of the conversation.
func (s *Server) resumePoint(r *http.Request, conversation int64) (int64, error) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	if raw == "" {
		return s.store.LatestEventID(r.Context(), conversation)
	}
	u, err := uuid.Parse(raw)
	if err != nil {
		return 0, &httpError{http.StatusBadRequest, "bad event id"}
	}
	id, err := s.store.EventIDByUUID(r.Context(), conversation, u)
	if db.IsNotFound(err) {
		return 0, &httpError{http.StatusBadRequest, "unknown event id"}
	}
	return id, err
}
