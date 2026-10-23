package httpapi

import (
	"net/http"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/types"
)

type approveRequest struct {
	CallID  string `json:"call_id"`
	Approve bool   `json:"approve"`
	Result  string `json:"result"` // optional text the model sees when rejected
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request, g *db.GrantRow) error {
	var req approveRequest
	if err := bindJSON(r, &req); err != nil {
		return err
	}
	if req.CallID == "" {
		return &httpError{http.StatusBadRequest, "call_id is required"}
	}
	err := s.svc.Approve(r.Context(), g.ConversationID, g.ActorID, types.ApprovalDecided{
		CallID: req.CallID, Approve: req.Approve, Result: types.Clean(req.Result)})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, struct{}{})
}
