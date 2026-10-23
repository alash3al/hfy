package httpapi

import (
	"net/http"

	"github.com/alash3al/hfy/internal/db"
)

// stop cancels the active turn and queued ones; the conversation and history stay.
func (s *Server) stop(w http.ResponseWriter, r *http.Request, g *db.GrantRow) error {
	if err := s.svc.Stop(r.Context(), g.ConversationID, g.ActorID); err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, struct{}{})
}
