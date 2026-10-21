package httpapi

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// stop cancels the active turn and queued ones; the conversation and history stay.
func (s *Server) stop(c *echo.Context) error {
	g := grantOf(c)
	if err := s.svc.Stop(c.Request().Context(), g.ConversationID, g.ActorID); err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, struct{}{})
}
