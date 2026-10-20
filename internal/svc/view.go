package svc

import (
	"encoding/json"

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
	Decided   map[string]types.ApprovalDecided
}

func ViewOf(events []db.EventRow) TurnView {
	v := TurnView{Done: map[string]bool{}, Requested: map[string]bool{}, Decided: map[string]types.ApprovalDecided{}}
	for _, e := range events {
		switch e.Type {
		case types.EventTurnState:
			var s types.TurnStateEvent
			if json.Unmarshal(e.Data, &s) == nil {
				v.State = s.State
			}
		case types.EventTurnCancelRequested:
			v.Cancelled = true
		case types.EventToolCall:
			var c types.ToolCall
			if json.Unmarshal(e.Data, &c) == nil {
				v.Calls = append(v.Calls, c)
			}
		case types.EventToolResult:
			var r types.ToolResult
			if json.Unmarshal(e.Data, &r) == nil {
				v.Done[r.CallID] = true
			}
		case types.EventToolCallInterrupted:
			var r types.ToolCallInterrupted
			if json.Unmarshal(e.Data, &r) == nil {
				v.Done[r.CallID] = true
			}
		case types.EventApprovalRequested:
			var a types.ApprovalRequested
			if json.Unmarshal(e.Data, &a) == nil {
				v.Requested[a.CallID] = true
			}
		case types.EventApprovalDecided:
			var a types.ApprovalDecided
			if json.Unmarshal(e.Data, &a) == nil {
				v.Decided[a.CallID] = a
			}
		}
	}
	return v
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
