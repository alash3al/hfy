package httpapi

import (
	"cmp"
	"net/http"
	"uuid"

	"github.com/labstack/echo/v5"

	"github.com/alash3al/hfy/internal/config"
)

type createSessionRequest struct {
	Agent string `json:"agent"` // the configured agent that answers in it; default main
}

type createSessionResponse struct {
	ID    uuid.UUID `json:"id"`
	Agent string    `json:"agent"`
}

// createSession is server to server: your backend stores the uuid and hands it
// to clients. A session is one room, so its agent is chosen here, once, by
// the backend; clients cannot change it.
func (s *Server) createSession(c *echo.Context) error {
	var req createSessionRequest
	if err := c.Bind(&req); err != nil { // an empty body is fine: main
		return echo.NewHTTPError(http.StatusBadRequest, "invalid json body")
	}
	agent := cmp.Or(req.Agent, config.MainAgent)
	if _, ok := s.cfg.AgentNamed(agent); !ok {
		return echo.NewHTTPError(http.StatusBadRequest, "unknown agent")
	}
	sess, err := s.store.CreateSession(c.Request().Context(), agent)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, createSessionResponse{ID: sess.UUID, Agent: sess.Agent})
}
