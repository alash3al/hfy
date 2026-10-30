package svc

import (
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/types"
)

// TurnView is a turn's state folded from its events: the one reading of the
// log that both the API side (approvals, stop) and the worker (resume) use.
type TurnView struct {
	State     types.TurnState
	Cancelled bool // a stop was requested
	Calls     []types.ToolCall
	Done      map[string]bool // call ids with a result or an interrupted marker
	Requested map[string]bool // call ids waiting on (or given) an approval
	// RequestData holds each approval request's extra payload (a tool's
	// questions, card and held result) keyed by call id.
	RequestData map[string]types.ApprovalRequested
	Decided     map[string]types.ApprovalDecided
}

func ViewOf(events []db.EventRow) (TurnView, error) {
	v := TurnView{Done: map[string]bool{}, Requested: map[string]bool{},
		RequestData: map[string]types.ApprovalRequested{}, Decided: map[string]types.ApprovalDecided{}}
	var err error
	for _, e := range events {
		switch e.Type {
		case types.EventTurnState:
			var s types.TurnStateEvent
			s, err = e.Payload[types.TurnStateEvent]()
			v.State = s.State
		case types.EventTurnCancelRequested:
			v.Cancelled = true
		case types.EventToolCall:
			var c types.ToolCall
			c, err = e.Payload[types.ToolCall]()
			v.Calls = append(v.Calls, c)
		case types.EventToolResult:
			var r types.ToolResult
			r, err = e.Payload[types.ToolResult]()
			v.Done[r.CallID] = true
		case types.EventToolCallInterrupted:
			var r types.ToolCallInterrupted
			r, err = e.Payload[types.ToolCallInterrupted]()
			v.Done[r.CallID] = true
		case types.EventApprovalRequested:
			var a types.ApprovalRequested
			a, err = e.Payload[types.ApprovalRequested]()
			v.Requested[a.CallID] = true
			v.RequestData[a.CallID] = a
		case types.EventApprovalDecided:
			var a types.ApprovalDecided
			a, err = e.Payload[types.ApprovalDecided]()
			v.Decided[a.CallID] = a
		}
		if err != nil {
			return v, err
		}
	}
	return v, nil
}

// Resumable reports whether a paused turn has a decision waiting to be applied.
func (v TurnView) Resumable() bool {
	for _, c := range v.Calls {
		if _, ok := v.Decided[c.CallID]; ok && !v.Done[c.CallID] {
			return true
		}
	}
	return false
}
