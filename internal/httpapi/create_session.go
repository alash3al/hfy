package httpapi

import (
	"net/http"
	"uuid"

	"github.com/labstack/echo/v5"
)

type createSessionResponse struct {
	ID uuid.UUID `json:"id"`
}

// createSession is server to server: your backend stores the uuid and hands it
// to clients.
func (s *Server) createSession(c *echo.Context) error {
	sess, err := s.store.CreateSession(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, createSessionResponse{ID: sess.UUID})
}
