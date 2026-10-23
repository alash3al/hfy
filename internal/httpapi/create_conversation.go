package httpapi

import (
	"net/http"
	"uuid"
)

type createConversationResponse struct {
	ID uuid.UUID `json:"id"`
}

// createConversation is server to server: your backend stores the uuid and hands it
// to clients.
func (s *Server) createConversation(w http.ResponseWriter, r *http.Request) error {
	conv, err := s.store.CreateConversation(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, createConversationResponse{ID: conv.UUID})
}
