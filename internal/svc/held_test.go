package svc

import (
	"testing"

	"github.com/alash3al/hfy/internal/types"
)

func heldReq() types.ApprovalRequested {
	return types.ApprovalRequested{Held: &types.HeldResult{Content: "your pizza"}}
}

// HeldResultText is the one tool_result the model ever gets for a held call,
// whichever side ends up writing it (svc ending a turn, agent resuming).
func TestHeldResultText(t *testing.T) {
	for _, c := range []struct {
		name    string
		req     types.ApprovalRequested
		d       types.ApprovalDecided
		decided bool
		want    string
	}{
		{"answered", heldReq(), types.ApprovalDecided{Approve: true, Answers: []byte(`{"size":"large"}`)}, true,
			"your pizza\n\n" + `{"answers":{"size":"large"}}`},
		{"declined", heldReq(), types.ApprovalDecided{Approve: false}, true,
			"your pizza\n\nThe user declined to answer."},
		{"timed out", heldReq(), types.ApprovalDecided{Approve: false, Reason: "timeout"}, true,
			"your pizza\n\nThe user did not answer in time."},
		{"approved without answers", heldReq(), types.ApprovalDecided{Approve: true}, true,
			"your pizza\n\nThe user declined to answer."},
		{"unusable answers", heldReq(), types.ApprovalDecided{Approve: true, Answers: []byte(`[1]`)}, true,
			"your pizza\n\nThe user declined to answer."},
		{"stopped before answering", heldReq(), types.ApprovalDecided{}, false,
			"your pizza\n\nThe user stopped before answering."},
		{"silent result, answered", types.ApprovalRequested{Held: &types.HeldResult{}},
			types.ApprovalDecided{Approve: true}, true, "The user declined to answer."},
		{"no held result at all (defensive)", types.ApprovalRequested{}, types.ApprovalDecided{}, false,
			"The user stopped before answering."},
	} {
		if got := HeldResultText(c.req, c.d, c.decided); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// The notes are the exact strings exported to the agent: no padding, ever.
	for _, c := range []struct{ have, want string }{
		{PromptDeclined, "The user declined to answer."},
		{PromptTimeout, "The user did not answer in time."},
		{PromptStopped, "The user stopped before answering."},
	} {
		if c.have != c.want {
			t.Errorf("prompt = %q, want %q", c.have, c.want)
		}
	}
}
