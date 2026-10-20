package httpapi

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// stop cancels the active turn and queued ones; the session and history stay.
func (s *Server) stop(c *echo.Context) error {
	cl := callerOf(c)
	if err := s.svc.Stop(c.Request().Context(), cl.session.ID, cl.actorID); err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, struct{}{})
}
