package httpapi

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/alash3al/hfy/internal/types"
)

type approveRequest struct {
	CallID  string `json:"call_id"`
	Approve bool   `json:"approve"`
	Result  string `json:"result"` // optional text the model sees when rejected
}

func (s *Server) approve(c *echo.Context) error {
	cl := callerOf(c)
	var req approveRequest
	if err := c.Bind(&req); err != nil || req.CallID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "call_id is required")
	}
	err := s.svc.Approve(c.Request().Context(), cl.session.ID, cl.actorID, types.ApprovalDecided{
		CallID: req.CallID, Approve: req.Approve, Result: types.Clean(req.Result)})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, struct{}{})
}
