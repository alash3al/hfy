package httpapi

import (
	"net/http"
	"strings"
	"uuid"

	"github.com/alash3al/hfy/internal/db"
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

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request, g *db.GrantRow) error {
	var req postMessageRequest
	if err := bindJSON(r, &req); err != nil {
		return err
	}
	req.Text = types.Clean(req.Text)
	if strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0 {
		return &httpError{http.StatusBadRequest, "text or attachments required"}
	}
	agent, _ := s.cfg.AgentNamed(g.Agent) // checked when the grant was resolved
	if err := svc.ValidateAttachments(s.cfg, agent, req.Attachments); err != nil {
		return err
	}
	vars, err := svc.ResolveVars(agent.Vars, g.Vars)
	if err != nil {
		return err
	}
	turn, err := s.svc.Post(r.Context(), svc.PostInput{
		ConversationID: g.ConversationID, Actor: g.ActorID, Scope: g.Scope, Vars: vars, Agent: g.Agent,
		Text: req.Text, Attachments: req.Attachments,
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, postMessageResponse{Turn: turn})
}
