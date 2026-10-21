package httpapi

import (
	"net/http"
	"strings"
	"uuid"

	"github.com/labstack/echo/v5"

	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

type postMessageRequest struct {
	Text        string             `json:"text"`
	Attachments []types.Attachment `json:"attachments"`
}

type postMessageResponse struct {
	Turn uuid.UUID `json:"turn"`
}

func (s *Server) postMessage(c *echo.Context) error {
	cl := callerOf(c)
	var req postMessageRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid json body")
	}
	req.Text = types.Clean(req.Text)
	if strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "text or attachments required")
	}
	agent, _ := s.cfg.AgentNamed(cl.agent) // checked when the caller was resolved
	if err := svc.ValidateAttachments(s.cfg, agent, req.Attachments); err != nil {
		return err
	}
	vars, err := svc.ResolveVars(agent.Vars, cl.vars)
	if err != nil {
		return err
	}
	turn, err := s.svc.Post(c.Request().Context(), svc.PostInput{
		ConversationID: cl.conversation.ID, Actor: cl.actorID, Scope: cl.scope, Vars: vars, Agent: cl.agent,
		Text: req.Text, Attachments: req.Attachments,
	})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, postMessageResponse{Turn: turn})
}
