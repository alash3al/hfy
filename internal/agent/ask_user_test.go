package agent_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

const askQuestions = `{"questions":[{"id":"size","text":"Which size?","type":"single","options":[{"value":"m","label":"Medium"},{"value":"l","label":"Large"}]},
	{"id":"extras","text":"Extras?","type":"multi","options":[{"value":"gift"},{"value":"wrap"}]},
	{"id":"note","text":"Note to the seller?","type":"text","required":false,"placeholder":"optional"}]}`

func askCall(id string) fakeprov.Step {
	return fakeprov.Step{Calls: []fakeprov.Call{{ID: id, Name: "hfy__ask_user", Args: askQuestions}}}
}

// hfy.ask_user pauses the turn; the answers return to the model as the call's
// result, exactly {"answers":{...}}, and the turn runs on with them.
func TestAskUserPausesAndServesTheAnswers(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.AskUser = true
		c.Agent.Approval = &config.Approval{Tools: []string{"hfy.*"}, Timeout: config.Duration(time.Minute)} // an ask never pauses twice
	}, askCall("q"), fakeprov.Step{Content: "got it, ordering"})
	turn := e.post("order a shirt")
	e.waitState(turn, types.StateAwaitingApproval)
	if names := toolNames(e.fake.Reqs()[0]); !strings.Contains(names, "hfy__ask_user") {
		t.Fatalf("offered tools: %s", names)
	}
	calls := e.find(turn, types.EventToolCall)
	if len(calls) != 1 || calls[0]["name"] != "hfy.ask_user" {
		t.Fatalf("tool calls: %v", calls)
	}
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 1 {
		t.Fatalf("approval requested events: %d\n%s", n, e.dump(turn))
	}

	var answers struct {
		Size   string   `json:"size"`
		Extras []string `json:"extras"`
	}
	answers.Size, answers.Extras = "m", []string{"wrap"}
	raw, _ := json.Marshal(answers)
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "q", Approve: true, Answers: raw}); err != nil {
		t.Fatal(err)
	}
	e.waitState(turn, types.StateCompleted)

	want := `{"answers":{"extras":["wrap"],"size":"m"}}`
	if r := e.find(turn, types.EventToolResult); len(r) != 1 || r[0]["content"] != want {
		t.Fatalf("tool results: %v, want %s\n%s", r, want, e.dump(turn))
	}
	reqs := e.fake.Reqs()
	if len(reqs) != 2 {
		t.Fatalf("model calls: %d", len(reqs))
	}
	body, _ := json.Marshal(reqs[1]["messages"])
	sans := strings.ReplaceAll(string(body), `\"`, `"`)
	if !strings.Contains(sans, want) {
		t.Fatalf("the model must see the answers: %s", body)
	}
}

// Declining (not answering) lets the turn go on with that text back.
func TestAskUserDeclined(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Agent.AskUser = true },
		askCall("q"), fakeprov.Step{Content: "very well, sending as is"})
	turn := e.post("go")
	e.waitState(turn, types.StateAwaitingApproval)
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "q", Approve: false}); err != nil {
		t.Fatal(err)
	}
	e.waitState(turn, types.StateCompleted)
	if r := e.find(turn, types.EventToolResult); len(r) != 1 || r[0]["content"] != "The user declined to answer." {
		t.Fatalf("declined result: %v", r)
	}
}

// An ask times out on the agent's approval.timeout; its text says so.
func TestAskUserTimeout(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.AskUser = true
		c.Agent.Approval = &config.Approval{Timeout: config.Duration(300 * time.Millisecond)}
	}, askCall("q"), fakeprov.Step{Content: "rms, going ahead"})
	turn := e.post("go")
	e.waitState(turn, types.StateCompleted)
	d := e.find(turn, types.EventApprovalDecided)
	if len(d) != 1 || d[0]["reason"] != "timeout" || d[0]["approve"] != false {
		t.Fatalf("decided: %v", d)
	}
	if r := e.find(turn, types.EventToolResult); len(r) != 1 || r[0]["content"] != "The user did not answer in time." {
		t.Fatalf("timeout result: %v", r)
	}
}

// A malformed call is an error result: the turn never pauses on it.
func TestAskUserInvalidArgsDoNotPause(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Agent.AskUser = true },
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "q", Name: "hfy__ask_user",
			Args: `{"questions":[{"id":"Size","text":"Which?","type":"single","options":[{"value":"m"},{"value":"l"}]}]}`}}},
		fakeprov.Step{Content: "fine, guessing then"})
	turn := e.post("go")
	e.waitState(turn, types.StateCompleted)
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 0 {
		t.Fatalf("paused on invalid questions\n%s", e.dump(turn))
	}
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["is_error"] != true || !strings.Contains(r[0]["content"].(string), "questions[0].id") {
		t.Fatalf("error result: %v", r)
	}
}

// Without the opt-in the tool does not exist: an unknown-tool error, never a
// pause. The owner rule still applies, and answers fail on calls that are
// not asks.
func TestAskUserRequiresOptInAndItsDecisionsHaveRules(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		c.Agent.Approval = &config.Approval{Tools: []string{"files.write_*"}, Timeout: config.Duration(time.Minute)}
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "no", Name: "hfy__ask_user", Args: askQuestions}}},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "w", Name: "files__write_file", Args: `{"text":"a.txt"}`}}},
		fakeprov.Step{Content: "done"})
	turn := e.post("go")
	e.waitState(turn, types.StateAwaitingApproval) // paused on files.write_file, never on the ask
	ask := e.find(turn, types.EventToolCall)
	if len(ask) != 2 || ask[0]["name"] != "hfy__ask_user" || ask[1]["name"] != "files.write_file" {
		t.Fatalf("tool calls: %v", ask)
	}
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["call_id"] != "no" || r[0]["is_error"] != true ||
		!strings.Contains(r[0]["content"].(string), "unknown tool") {
		t.Fatalf("ask result: %v", r)
	}
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 1 {
		t.Fatalf("approval requests: %d\n%s", n, e.dump(turn))
	}

	// answers only on ask calls
	err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "w", Approve: true, Answers: json.RawMessage(`{"size":"m"}`)})
	var bad *svc.BadRequestError
	if err == nil || !errors.As(err, &bad) || !strings.Contains(err.Error(), "answers are only accepted") {
		t.Fatalf("answers on a non-ask call: %v", err)
	}
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.actor("u2"), types.ApprovalDecided{CallID: "w", Approve: true}); !errors.Is(err, svc.ErrNotTurnOwner) {
		t.Fatalf("non-owner answered: %v", err)
	}
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "w", Approve: true}); err != nil {
		t.Fatal(err)
	}
	e.waitState(turn, types.StateCompleted)
}
