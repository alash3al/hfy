package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

func uiServer(c *config.Config) {
	srv := c.MCP["files"]
	srv.UI = &config.UI{ImageHosts: []string{"imgs.example"}}
	c.MCP["files"] = srv
}

// A card a ui-opted-in server returns rides the tool_result event; a tool
// result with questions keeps them on the result for a later pause.
func TestUIRidesTheToolResult(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		uiServer(c)
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "u", Name: "files__ui"}}},
		fakeprov.Step{Content: "enjoy"},
	)
	turn := e.post("one margherita")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["content"] != "here is your order" {
		t.Fatalf("tool result: %v\n%s", r, e.dump(turn))
	}
	card, _ := r[0]["ui"].(map[string]any)
	if card == nil || card["title"] != "Margherita" {
		t.Fatalf("card: %v", r[0]["ui"])
	}
}

// A card from a server that has no ui config never reaches a client.
func TestUIWithoutConfigIsStripped(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Agent.MCP = []string{"files"} },
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "u", Name: "files__ui"}}},
		fakeprov.Step{Content: "enjoy"},
	)
	turn := e.post("one margherita")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["ui"] != nil {
		t.Fatalf("ui survived the strip: %v", r)
	}
}

// A failed call for its ui: an error strips the card too.
func TestUIDiesWithTheCall(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		uiServer(c)
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "b", Name: "files__ui_boom"}}},
		fakeprov.Step{Content: "sorry"},
	)
	turn := e.post("try it")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || !r[0]["is_error"].(bool) || r[0]["ui"] != nil {
		t.Fatalf("error result keeps its ui: %v", r)
	}
}

// A contentless call's card rides the result like any other.
func TestUISilentCardRidesToo(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		uiServer(c)
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "s", Name: "files__ui_silent"}}},
		fakeprov.Step{Content: "quiet it is"},
	)
	turn := e.post("anything quiet")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["content"] != "" {
		t.Fatalf("silent result: %v", r)
	}
	card, _ := r[0]["ui"].(map[string]any)
	if card == nil || card["title"] != "Quiet card" {
		t.Fatalf("silent card: %v", r[0]["ui"])
	}
}

// askSettings opts a turn into asking: the agent may call hfy.ask_user, so its
// tools may ask the viewer questions of their own.
func askSettings(c *config.Config) {
	c.Agent.MCP = []string{"files"}
	c.Agent.AskUser = true
	uiServer(c)
}

// A tool that returns questions pauses the turn on its own call; answering
// hands the model its held result joined with the answers, one tool_result.
func TestToolQuestionsPauseAndServeTheAnswers(t *testing.T) {
	e := setup(t, askSettings,
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask"}}},
		fakeprov.Step{Content: "size noted"},
	)
	turn := e.post("order it")
	e.waitState(turn, types.StateAwaitingApproval)
	req := e.find(turn, types.EventApprovalRequested)
	if len(req) != 1 {
		t.Fatalf("approval requests: %d\n%s", len(req), e.dump(turn))
	}
	held, _ := req[0]["held"].(map[string]any)
	if held == nil || held["content"] != "your pizza, pick a size" {
		t.Fatalf("held result: %v", req[0]["held"])
	}
	if card, _ := req[0]["ui"].(map[string]any); card == nil || card["title"] != "Order" {
		t.Fatalf("request card: %v", req[0]["ui"])
	}
	if _, ok := req[0]["questions"]; !ok {
		t.Fatalf("request carries no questions: %v", req[0])
	}
	if n := len(e.find(turn, types.EventToolResult)); n != 0 {
		t.Fatalf("the held call must not surface a result yet\n%s", e.dump(turn))
	}
	raw := []byte(`{"size":"large"}`)
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "a", Approve: true, Answers: raw}); err != nil {
		t.Fatal(err)
	}
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["content"] != "your pizza, pick a size\n\n"+`{"answers":{"size":"large"}}` {
		t.Fatalf("answered tool_result: %v\n%s", r, e.dump(turn))
	}
	if card, _ := r[0]["ui"].(map[string]any); card == nil || card["title"] != "Order" {
		t.Fatalf("result card: %v", r[0]["ui"])
	}
	msgs, _ := json.Marshal(e.fake.Reqs()[1]["messages"]) // the model saw it all
	if !strings.Contains(string(msgs), "large") {
		t.Fatalf("model did not see the answers: %s", msgs)
	}
}

// Declining and timing out both leave the held result as the tool_result, with
// the plain prompt instead of answers.
func TestToolQuestionsDeclinedOrTimedOut(t *testing.T) {
	t.Run("declined", func(t *testing.T) {
		e := setup(t, askSettings,
			fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask"}}},
			fakeprov.Step{Content: "okay, skipping"},
		)
		turn := e.post("order it")
		e.waitState(turn, types.StateAwaitingApproval)
		if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "a", Approve: false}); err != nil {
			t.Fatal(err)
		}
		e.waitState(turn, types.StateCompleted)
		r := e.find(turn, types.EventToolResult)
		want := "your pizza, pick a size\n\nThe user declined to answer."
		if len(r) != 1 || r[0]["content"] != want {
			t.Fatalf("declined tool_result: %v\n%s", r, e.dump(turn))
		}
	})
	t.Run("timed out", func(t *testing.T) {
		e := setup(t, func(c *config.Config) {
			askSettings(c)
			c.Agent.Approval = &config.Approval{Timeout: config.Duration(300 * time.Millisecond)}
		},
			fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask"}}},
			fakeprov.Step{Content: "okay, skipping"},
		)
		turn := e.post("order it")
		e.waitState(turn, types.StateCompleted)
		r := e.find(turn, types.EventToolResult)
		want := "your pizza, pick a size\n\nThe user did not answer in time."
		if len(r) != 1 || r[0]["content"] != want {
			t.Fatalf("timed-out tool_result: %v\n%s", r, e.dump(turn))
		}
	})
}

// A stop after the user answered but before the resume ran must not lose the
// answers: the model gets exactly one tool_result, the held result joined
// with them, and the resume never re-runs the tool.
func TestToolQuestionsAnsweredThenStopped(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		askSettings(c)
		c.Queue.Workers = 0 // this test plays the worker itself
	}, fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask"}}},
		fakeprov.Step{Content: "size noted"},
	)
	turn := e.post("order it")
	if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "a", Approve: true, Answers: []byte(`{"size":"large"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.Svc.Stop(e.ctx, e.conv.ID, e.user); err != nil { // before the resume can run
		t.Fatal(err)
	}
	e.waitState(turn, types.StateStopped)
	r := e.find(turn, types.EventToolResult)
	want := "your pizza, pick a size\n\n" + `{"answers":{"size":"large"}}`
	if len(r) != 1 || r[0]["content"] != want {
		t.Fatalf("answered-then-stopped tool_result: %v\n%s", r, e.dump(turn))
	}
	if n := len(e.find(turn, types.EventToolCallInterrupted)); n != 0 {
		t.Fatalf("the answered call was marked interrupted\n%s", e.dump(turn))
	}
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 1 {
		t.Fatalf("approval requests: %d\n%s", n, e.dump(turn))
	}
}

// The stop may land while the loop is still holding: the cancel is already in
// the log when the loop asks for the approval. The computed result is then
// stored once as dropped, one tool_result, not an interrupted marker.
func TestToolQuestionsStoppedMidHold(t *testing.T) {
	var (
		store  *db.Store
		convID int64
		turn   int64
	)
	stop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		user, _ := store.FindOrCreateActor(context.Background(), store.Pool, types.ActorExternal, "u1")
		store.AppendEvent(context.Background(), store.Pool, db.NewEvent{ConversationID: convID,
			ParentID: &turn, ActorID: user, Scope: "u1", Payload: types.TurnCancelRequested{}})
	}))
	defer stop.Close()
	e := setup(t, func(c *config.Config) {
		askSettings(c)
		c.Queue.Workers = 0
		srv := c.MCP["files"]
		srv.Environment["HFY_STOP_URL"] = stop.URL
		c.MCP["files"] = srv
	}, fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask_stop"}}},
		fakeprov.Step{Content: "size noted"},
	)
	store, convID = e.store, e.conv.ID
	turn = e.post("order it")
	if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	e.waitState(turn, types.StateStopped)
	r := e.find(turn, types.EventToolResult)
	want := "your pizza, pick a size\n\nThe user stopped before answering."
	if len(r) != 1 || r[0]["content"] != want {
		t.Fatalf("mid-hold stopped tool_result: %v\n%s", r, e.dump(turn))
	}
	if n := len(e.find(turn, types.EventToolCallInterrupted)); n != 0 {
		t.Fatalf("the held call was marked interrupted\n%s", e.dump(turn))
	}
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 0 {
		t.Fatalf("no approval may be requested after the stop\n%s", e.dump(turn))
	}
}

// Without the ask_user opt-in a tool's questions are dropped with only the
// card kept: the result is the tool's text, the card rides it, nothing pauses.
func TestToolQuestionsWithoutAskUser(t *testing.T) {
	e := setup(t, func(c *config.Config) { // no AskUser
		c.Agent.MCP = []string{"files"}
		uiServer(c)
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask"}}},
		fakeprov.Step{Content: "size, whatever"},
	)
	turn := e.post("order it")
	e.waitState(turn, types.StateCompleted)
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 0 {
		t.Fatalf("a non-asking agent must not pause\n%s", e.dump(turn))
	}
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["content"] != "your pizza, pick a size" {
		t.Fatalf("tool result: %v\n%s", r, e.dump(turn))
	}
	if card, _ := r[0]["ui"].(map[string]any); card == nil || card["title"] != "Order" {
		t.Fatalf("card: %v", r[0]["ui"])
	}
}

// A sub-agent never pauses on a tool's questions either: its run is not
// stored, so the card is alone with the questions and there is no approval.
func TestSubagentGetsNoPause(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		uiServer(c)
		c.Subagents = map[string]config.Subagent{"orders": {
			Description: "Handles orders", Model: "fake/m", System: "Orders.",
			MCP: []string{"files"},
		}}
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "r", Name: "hfy__orders", Args: `{"task":"order it"}`}}},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask"}}},
		fakeprov.Step{Content: "picked"},
		fakeprov.Step{Content: "order placed"},
	)
	turn := e.post("place it")
	e.waitState(turn, types.StateCompleted)
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 0 {
		t.Fatalf("a sub-agent must not pause\n%s", e.dump(turn))
	}
	if e.state(turn) == types.StateAwaitingApproval {
		t.Fatalf("paused\n%s", e.dump(turn))
	}
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["content"] != "picked" {
		t.Fatalf("tool result: %v\n%s", r, e.dump(turn))
	}
}

// A tool may ship only questions, no card part: the turn pauses the same way
// and the model gets the held text joined with the answers.
func TestToolQuestionsOnlyPause(t *testing.T) {
	e := setup(t, askSettings,
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_questions"}}},
		fakeprov.Step{Content: "noted"},
	)
	turn := e.post("which way")
	e.waitState(turn, types.StateAwaitingApproval)
	req := e.find(turn, types.EventApprovalRequested)
	if len(req) != 1 || req[0]["ui"] != nil {
		t.Fatalf("questions-only request: %v\n%s", req, e.dump(turn))
	}
	if _, ok := req[0]["questions"]; !ok {
		t.Fatalf("request carries no questions: %v", req[0])
	}
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "a", Approve: true, Answers: []byte(`{"size":"large"}`)}); err != nil {
		t.Fatal(err)
	}
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["content"] != "just asking\n\n"+`{"answers":{"size":"large"}}` {
		t.Fatalf("questions-only tool_result: %v\n%s", r, e.dump(turn))
	}
}

// Answers that fail the stored questions refuse the decision; only the
// question's input is the client's data. The pause stands.
func TestToolQuestionsRejectBadAnswers(t *testing.T) {
	e := setup(t, askSettings,
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask"}}},
		fakeprov.Step{Content: "size noted"},
	)
	turn := e.post("order it")
	e.waitState(turn, types.StateAwaitingApproval)
	err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "a", Approve: true, Answers: []byte(`{"wrong_id":"big"}`)})
	if _, ok := errors.AsType[*svc.BadRequestError](err); !ok {
		t.Fatalf("want a bad-answers 400, got %v", err)
	}
	if e.state(turn) != types.StateAwaitingApproval {
		t.Fatalf("a refused decision must still pause: %s", e.state(turn))
	}
}

// A stop while questions are open ends the turn with the held call's
// tool_result as an unanswered one; the decision is gone.
func TestToolQuestionsStopBeforeAnswering(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		askSettings(c)
		c.Queue.Workers = 0 // this test plays the worker itself
	}, fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask"}}},
		fakeprov.Step{Content: "size noted"},
	)
	turn := e.post("order it")
	if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if e.state(turn) != types.StateAwaitingApproval {
		t.Fatalf("state: %s", e.state(turn))
	}
	if err := e.rt.Svc.Stop(e.ctx, e.conv.ID, e.user); err != nil {
		t.Fatal(err)
	}
	e.waitState(turn, types.StateStopped)
	r := e.find(turn, types.EventToolResult)
	want := "your pizza, pick a size\n\nThe user stopped before answering."
	if len(r) != 1 || r[0]["content"] != want {
		t.Fatalf("stopped tool_result: %v\n%s", r, e.dump(turn))
	}
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "a", Approve: true}); !errors.Is(err, svc.ErrNoPendingApproval) {
		t.Fatalf("approve after the stop: %v", err)
	}
}

// The resumed worker serves the held result instead of re-running the call,
// even when it only sees the turn on its second attempt.
func TestHeldCallIsServedNotReExecutedOnRetry(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		askSettings(c)
		c.Queue.Workers = 0
	}, fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask"}}},
		fakeprov.Step{Content: "size noted"},
	)
	turn := e.post("order it")
	if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "a", Approve: true, Answers: []byte(`{"size":"large"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 2}); err != nil {
		t.Fatal(err)
	}
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["content"] != "your pizza, pick a size\n\n"+`{"answers":{"size":"large"}}` {
		t.Fatalf("retry tool_result: %v\n%s", r, e.dump(turn))
	}
	if n := len(e.find(turn, types.EventToolCallInterrupted)); n != 0 {
		t.Fatalf("the held call was marked interrupted\n%s", e.dump(turn))
	}
}

// An approval-gated tool cannot pause again on its questions: the approved
// resume runs it once and the questions are dropped with a warning.
func TestApprovedToolCannotAsk(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		uiServer(c)
		srv := c.MCP["files"]
		srv.Approval = []string{"ui_ask"}
		c.MCP["files"] = srv
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "a", Name: "files__ui_ask"}}},
		fakeprov.Step{Content: "ordered"},
	)
	turn := e.post("order it")
	e.waitState(turn, types.StateAwaitingApproval)
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "a", Approve: true}); err != nil {
		t.Fatal(err)
	}
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["content"] != "your pizza, pick a size" {
		t.Fatalf("approved then asked: %v\n%s", r, e.dump(turn))
	}
	card, _ := r[0]["ui"].(map[string]any)
	if card == nil || card["title"] != "Order" {
		t.Fatalf("card: %v", r[0]["ui"])
	}
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 1 {
		t.Fatalf("approval requests: %d\n%s", n, e.dump(turn))
	}
}
