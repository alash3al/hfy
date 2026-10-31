package httpapi

import (
	"net/http"

	"github.com/alash3al/hfy/internal/db"
)

type conversationConfigResponse struct {
	// Uploads is what the embed page's config injects there: the composer's
	// limits, so a client offers attachments only within them. Null when no
	// filesystem is configured.
	Uploads *embedUploads `json:"uploads"`
}

// conversationConfig is the config a client (which has no page to embed)
// needs from the API: only what is safe for any grant holder.
func (s *Server) conversationConfig(w http.ResponseWriter, _ *http.Request, _ *db.GrantRow) error {
	return writeJSON(w, http.StatusOK, conversationConfigResponse{Uploads: s.embedUploads()})
}
