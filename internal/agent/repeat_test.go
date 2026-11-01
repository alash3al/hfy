package agent_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// priorCall records a finished call of the main agent in turn, as an earlier
// run of the turn did before it was resumed.
func (e *env) priorCall(turn int64, id, name, args string) {
	e.t.Helper()
	main, err := e.store.FindOrCreateActor(e.ctx, e.store.Pool, types.ActorAgent, "main")
	if err != nil {
		e.t.Fatal(err)
	}
	row, err := e.store.EventByID(e.ctx, e.store.Pool, turn)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range []types.Payload{
		types.ToolCall{Name: name, CallID: id, Arguments: json.RawMessage(args)},
		types.ToolResult{CallID: id, Content: "earlier"},
	} {
		if _, err := e.store.AppendEvent(e.ctx, e.store.Pool, db.NewEvent{ConversationID: e.conv.ID, ParentID: &turn,
			ActorID: main, Scope: row.Scope, Payload: p}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// The guard counts a resumed turn's earlier calls at the same thresholds as a
// turn that never stopped. Stored arguments come back from jsonb with their
// keys reordered, and the key must still match.
func TestRepeatGuardSurvivesResume(t *testing.T) {
	for _, c := range []struct {
		name, tool, priorName, priorArgs, liveArgs string
		prior                                      int
		state                                      types.TurnState
		want                                       string
	}{
		{name: "fresh turn runs the call", tool: "files__echo", priorName: "files.echo",
			priorArgs: `{"text":"x"}`, liveArgs: `{"text":"x"}`, state: types.StateCompleted, want: "echo: x"},
		{name: "third identical call is refused", tool: "files__echo", priorName: "files.echo", prior: 2,
			priorArgs: `{"text":"x"}`, liveArgs: `{"text":"x"}`, state: types.StateCompleted, want: "already made this exact call"},
		{name: "sixth identical call ends the turn", tool: "files__echo", priorName: "files.echo", prior: 5,
			priorArgs: `{"text":"x"}`, liveArgs: `{"text":"x"}`, state: types.StateFailed, want: "loop detected"},
		{name: "other arguments are another call", tool: "files__echo", priorName: "files.echo", prior: 5,
			priorArgs: `{"text":"x"}`, liveArgs: `{"text":"y"}`, state: types.StateCompleted, want: "echo: y"},
		{name: "key order in storage does not matter", tool: "hfy__tool_search", priorName: "hfy.tool_search", prior: 2,
			priorArgs: `{"query":"echo","limit":3}`, liveArgs: `{"query":"echo","limit":3}`, state: types.StateCompleted,
			want: "already made this exact call"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, func(cfg *config.Config) { cfg.Queue.Workers = 0 }, // this test runs the turn itself
				fakeprov.Step{Calls: []fakeprov.Call{{ID: "now", Name: c.tool, Args: c.liveArgs}}}, fakeprov.Step{Content: "done"})
			turn := e.post("go")
			for i := range c.prior {
				e.priorCall(turn, fmt.Sprintf("p%d", i), c.priorName, c.priorArgs)
			}
			if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 2}); err != nil {
				t.Fatal(err)
			}
			if e.state(turn) != c.state {
				t.Fatalf("state %s, want %s\n%s", e.state(turn), c.state, e.dump(turn))
			}
			states := e.find(turn, types.EventTurnState)
			if c.state == types.StateFailed {
				if r := states[len(states)-1]["reason"].(string); !strings.Contains(r, c.want) {
					t.Fatalf("reason %q, want %q", r, c.want)
				}
				return
			}
			results := e.find(turn, types.EventToolResult)
			if got := results[len(results)-1]["content"].(string); !strings.Contains(got, c.want) {
				t.Fatalf("live call result %q, want %q", got, c.want)
			}
		})
	}
}

// An approval pause counts once, and each resume keeps the count: the third
// identical write is refused without asking the viewer again.
func TestRepeatGuardSurvivesApprovalResumes(t *testing.T) {
	write := func(id string) fakeprov.Step {
		return fakeprov.Step{Calls: []fakeprov.Call{{ID: id, Name: "files__write_file", Args: `{"text":"a.txt"}`}}}
	}
	e := setup(t, func(c *config.Config) {
		c.Main().MCP = []string{"files"}
		c.Main().Approval = &config.Approval{Tools: []string{"files.write_*"}, Timeout: config.Duration(time.Minute)}
		c.Queue.Workers = 0
	}, write("w1"), write("w2"), write("w3"), fakeprov.Step{Content: "done"})
	turn := e.post("write it")
	run := func() {
		if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
			t.Fatal(err)
		}
	}
	approve := func(id string) {
		if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: id, Approve: true}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	approve("w1")
	run()
	if e.state(turn) != types.StateAwaitingApproval {
		t.Fatalf("second write must pause\n%s", e.dump(turn))
	}
	approve("w2")
	run()
	if e.state(turn) != types.StateCompleted {
		t.Fatalf("state %s, want completed\n%s", e.state(turn), e.dump(turn))
	}
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 2 {
		t.Fatalf("approval requests: %d, want 2 (the refused third call never asks)", n)
	}
	r := e.find(turn, types.EventToolResult)
	if len(r) != 3 || r[0]["content"] != "wrote a.txt" || r[1]["content"] != "wrote a.txt" ||
		!strings.Contains(r[2]["content"].(string), "already made this exact call") {
		t.Fatalf("results: %v", r)
	}
}

// An ask the viewer declines counts like any call: the same questions asked a
// third time are refused without pausing the turn for the viewer again.
func TestRepeatedAsksAreCounted(t *testing.T) {
	ask := func(id string) fakeprov.Step {
		return fakeprov.Step{Calls: []fakeprov.Call{{ID: id, Name: "hfy__ask_user", Args: askQuestions}}}
	}
	e := setup(t, func(c *config.Config) {
		c.Main().AskUser = true
		c.Queue.Workers = 0
	}, ask("q1"), ask("q2"), ask("q3"), fakeprov.Step{Content: "done"})
	turn := e.post("order")
	run := func() {
		if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"q1", "q2"} {
		run()
		if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: id, Approve: false}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if e.state(turn) != types.StateCompleted {
		t.Fatalf("state %s, want completed (the third ask is refused)\n%s", e.state(turn), e.dump(turn))
	}
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 2 {
		t.Fatalf("asks that paused: %d, want 2", n)
	}
	if r := e.find(turn, types.EventToolResult); len(r) != 3 || !strings.Contains(fmt.Sprint(r[2]["content"]), "already made this exact call") {
		t.Fatalf("third ask: %v", r)
	}
}

// Answered asks never count: a legit loop of the same question, answered each
// time ("Proceed with next item?"), runs to its end however often it repeats.
func TestAnsweredAsksNeverRepeatCount(t *testing.T) {
	steps := make([]fakeprov.Step, 0, 11)
	for i := range 10 {
		steps = append(steps, fakeprov.Step{Calls: []fakeprov.Call{{ID: fmt.Sprintf("q%d", i), Name: "hfy__ask_user", Args: askQuestions}}})
	}
	e := setup(t, func(c *config.Config) {
		c.Main().AskUser = true
		c.Queue.Workers = 0
	}, append(steps, fakeprov.Step{Content: "all proceeded"})...)
	turn := e.post("proceed through the list")
	answer := json.RawMessage(`{"size":"m","extras":["wrap"]}`)
	for i := range 10 {
		if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
			t.Fatal(err)
		}
		if e.state(turn) != types.StateAwaitingApproval {
			t.Fatalf("ask %d did not pause: state %s\n%s", i+1, e.state(turn), e.dump(turn))
		}
		if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: fmt.Sprintf("q%d", i), Approve: true, Answers: answer}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if e.state(turn) != types.StateCompleted {
		t.Fatalf("state %s, want completed\n%s", e.state(turn), e.dump(turn))
	}
	for _, r := range e.find(turn, types.EventToolResult) {
		if strings.Contains(fmt.Sprint(r["content"]), "already made") {
			t.Fatalf("an answered ask was refused: %v", r)
		}
	}
}

// Declined asks keep counting: the same question declined over and over ends
// the turn, the sixth identical ask being the one that does.
func TestDeclinedAsksEndTheTurn(t *testing.T) {
	steps := make([]fakeprov.Step, 0, 10)
	for i := range 10 {
		steps = append(steps, fakeprov.Step{Calls: []fakeprov.Call{{ID: fmt.Sprintf("q%d", i), Name: "hfy__ask_user", Args: askQuestions}}})
	}
	e := setup(t, func(c *config.Config) {
		c.Main().AskUser = true
		c.Queue.Workers = 0
	}, steps...)
	turn := e.post("proceed through the list")
	for i := range 10 {
		if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
			t.Fatal(err)
		}
		if e.state(turn) != types.StateAwaitingApproval {
			break
		}
		if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: fmt.Sprintf("q%d", i), Approve: false}); err != nil {
			t.Fatal(err)
		}
	}
	if e.state(turn) != types.StateFailed {
		t.Fatalf("state %s, want failed\n%s", e.state(turn), e.dump(turn))
	}
	states := e.find(turn, types.EventTurnState)
	if r := states[len(states)-1]["reason"].(string); !strings.Contains(r, "loop detected") {
		t.Fatalf("reason: %s", r)
	}
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 2 {
		t.Fatalf("asks that paused: %d, want 2 (the third to sixth are refused)", n)
	}
}

// A held result (a tool that asked its questions) counts once: its resume
// serves the result and does not count the call again, so the second identical
// call still runs and pauses, and only the third is refused.
func TestHeldResultCountsOnce(t *testing.T) {
	call := func(id string) fakeprov.Step {
		return fakeprov.Step{Calls: []fakeprov.Call{{ID: id, Name: "files__ui_ask"}}}
	}
	e := setup(t, func(c *config.Config) {
		askSettings(c)
		c.Queue.Workers = 0
	}, call("h1"), call("h2"), call("h3"), fakeprov.Step{Content: "done"})
	turn := e.post("order it")
	run := func() {
		if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
			t.Fatal(err)
		}
	}
	answer := []byte(`{"size":"large"}`)
	run()
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "h1", Approve: true, Answers: answer}); err != nil {
		t.Fatal(err)
	}
	run() // resumes h1, then the model asks for h2
	if e.state(turn) != types.StateAwaitingApproval {
		t.Fatalf("the second held call must pause after the resume: state %s\n%s", e.state(turn), e.dump(turn))
	}
	if n := len(e.find(turn, types.EventApprovalRequested)); n != 2 {
		t.Fatalf("held pauses: %d, want 2: the resumed call was counted twice\n%s", n, e.dump(turn))
	}
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: "h2", Approve: true, Answers: answer}); err != nil {
		t.Fatal(err)
	}
	run()
	if e.state(turn) != types.StateCompleted {
		t.Fatalf("state %s, want completed (the third identical call is refused)\n%s", e.state(turn), e.dump(turn))
	}
	if r := e.find(turn, types.EventToolResult); len(r) != 3 || !strings.Contains(fmt.Sprint(r[2]["content"]), "already made this exact call") {
		t.Fatalf("results: %v", r)
	}
}

// Counts belong to their turn and conversation: a new turn, or another
// conversation, starts at zero however often an earlier one repeated a call.
func TestRepeatCountsStayWithTheirTurn(t *testing.T) {
	x := `{"text":"x"}`
	call := func(id string) fakeprov.Step {
		return fakeprov.Step{Calls: []fakeprov.Call{{ID: id, Name: "files__echo", Args: x}}}
	}
	e := setup(t, func(c *config.Config) { c.Main().MCP = []string{"files"} },
		call("c1"), call("c2"), fakeprov.Step{Content: "one"},
		call("c3"), fakeprov.Step{Content: "two"},
		call("c4"), fakeprov.Step{Content: "three"},
	)
	t1 := e.post("first")
	e.waitState(t1, types.StateCompleted)
	t2 := e.post("second")
	e.waitState(t2, types.StateCompleted)

	other, err := e.store.CreateConversation(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	vars, _ := svc.ResolveVars(e.rt.Cfg.Main().Vars, nil)
	u, err := e.rt.Svc.Post(e.ctx, svc.PostInput{ConversationID: other.ID, Actor: e.user, Scope: "u1", Vars: vars, Text: "third"})
	if err != nil {
		t.Fatal(err)
	}
	t3, err := e.store.TurnIDByUUID(e.ctx, other.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	e.waitState(t3, types.StateCompleted)

	for _, turn := range []int64{t1, t2, t3} {
		r := e.find(turn, types.EventToolResult)
		if last := r[len(r)-1]["content"]; !strings.HasPrefix(fmt.Sprint(last), "echo: x") {
			t.Fatalf("turn %d: last call %v was refused", turn, last)
		}
	}
	if n := len(e.find(t1, types.EventToolResult)); n != 2 {
		t.Fatalf("two identical calls in one turn are both run: %d results", n)
	}
}
