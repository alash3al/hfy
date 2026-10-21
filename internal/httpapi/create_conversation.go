package httpapi

import (
	"net/http"
	"uuid"

	"github.com/labstack/echo/v5"
)

type createConversationResponse struct {
	ID uuid.UUID `json:"id"`
}

// createConversation is server to server: your backend stores the uuid and hands it
// to clients.
func (s *Server) createConversation(c *echo.Context) error {
	conv, err := s.store.CreateConversation(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, createConversationResponse{ID: conv.UUID})
}
