package httpapi

import (
	"context"
	"log/slog"
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
	vars, err := svc.ResolveVars(agent.Vars, g.Vars) // before copying files: nothing to undo if it fails
	if err != nil {
		return err
	}
	verified, posted, err := s.verifyFileAttachments(r, g, req.Attachments)
	if err != nil {
		return err
	}
	req.Attachments = verified
	turn, err := s.svc.Post(r.Context(), svc.PostInput{
		ConversationID: g.ConversationID, Actor: g.ActorID, Scope: g.Scope, Vars: vars, Agent: g.Agent,
		Text: req.Text, Attachments: req.Attachments, Files: posted,
	})
	if err != nil {
		// The event and its files index rolled back together, so the verified
		// copies nothing references are deleted. Nothing can act on a
		// deletion failure but log it (retention's slots sweep would not
		// catch these: they live under conversations/).
		ctx := context.WithoutCancel(r.Context()) // a client that hung up still leaves no orphans
		for _, f := range posted {
			if err := s.bucket.Delete(ctx, f.Key); err != nil {
				slog.Warn("deleting a verified copy after a failed post", "key", f.Key, "err", err)
			}
		}
		return err
	}
	return writeJSON(w, http.StatusAccepted, postMessageResponse{Turn: turn})
}
