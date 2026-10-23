package httpapi

import (
	"net/http"
	"strconv"
	"uuid"

	"github.com/labstack/echo/v5"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/types"
)

type historyTurn struct {
	ID     uuid.UUID     `json:"id"`
	Events []types.Event `json:"events"`
}

type historyResponse struct {
	Turns []historyTurn `json:"turns"` // oldest first within the page
	// Next is the cursor for older turns: pass it as ?before=. Null at the start.
	Next *uuid.UUID `json:"next"`
	// LiveAfter is where to continue: open /events?after=<live_after>.
	LiveAfter *uuid.UUID `json:"live_after"`
}

// history returns durable events grouped by turn, newest page first, keyset
// paginated by turn uuid.
func (s *Server) history(c *echo.Context, g *db.GrantRow) error {
	ctx := c.Request().Context()
	limit := 20
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return echo.NewHTTPError(http.StatusBadRequest, "limit must be 1..100")
		}
		limit = n
	}
	var before *int64
	if v := c.QueryParam("before"); v != "" {
		u, err := uuid.Parse(v)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "bad before cursor")
		}
		id, err := s.store.TurnIDByUUID(ctx, g.ConversationID, u)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "unknown before cursor")
		}
		before = &id
	}
	// Read the live cursor first: anything appended after it is replayed by /events.
	latest, err := s.store.LatestEventID(ctx, g.ConversationID)
	if err != nil {
		return err
	}
	ids, err := s.store.HistoryTurnIDs(ctx, g.ConversationID, before, limit)
	if err != nil {
		return err
	}
	resp := historyResponse{Turns: []historyTurn{}}
	if latest > 0 {
		ev, err := s.store.EventByID(ctx, s.store.Pool, latest)
		if err != nil {
			return err
		}
		resp.LiveAfter = &ev.UUID
	}
	if len(ids) > 0 {
		rows, err := s.store.HistoryEvents(ctx, g.ConversationID, ids)
		if err != nil {
			return err
		}
		byTurn := map[int64]*historyTurn{}
		var order []int64
		for _, r := range rows {
			t := byTurn[r.TurnID()]
			if t == nil {
				t = &historyTurn{Events: []types.Event{}}
				byTurn[r.TurnID()] = t
				order = append(order, r.TurnID())
			}
			if r.ParentID == nil {
				t.ID = r.UUID
			}
			t.Events = append(t.Events, r.Envelope())
		}
		for _, id := range order {
			resp.Turns = append(resp.Turns, *byTurn[id])
		}
		if len(ids) == limit {
			oldest := byTurn[ids[len(ids)-1]]
			resp.Next = &oldest.ID
		}
	}
	return c.JSON(http.StatusOK, resp)
}
