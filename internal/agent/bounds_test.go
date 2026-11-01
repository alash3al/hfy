package agent_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// addMessage appends a user message to a turn, as a late writer would.
func (e *env) addMessage(turn int64, text string) {
	e.t.Helper()
	row, err := e.store.EventByID(e.ctx, e.store.Pool, turn)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.store.AppendEvent(e.ctx, e.store.Pool, db.NewEvent{ConversationID: e.conv.ID, ParentID: &turn,
		ActorID: e.user, Scope: row.Scope, Payload: types.MessageText{Text: text}}); err != nil {
		e.t.Fatal(err)
	}
}

// The argument limit is exact: a call of maxArgBytes runs, one byte more is
// refused, and the event log keeps a marker instead of the arguments.
func TestArgumentsLimitIsExact(t *testing.T) {
	const limit = 64 << 10 // maxArgBytes
	pad := func(n int) string { return `{"text":"` + strings.Repeat("a", n-len(`{"text":""}`)) + `"}` }
	e := setup(t, func(c *config.Config) { c.Agent.MCP = []string{"files"} },
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "big", Name: "files__echo", Args: pad(limit + 1)}}},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "edge", Name: "files__echo", Args: pad(limit)}}},
		fakeprov.Step{Content: "done"})
	turn := e.post("go")
	e.waitState(turn, types.StateCompleted)

	r := e.find(turn, types.EventToolResult)
	if len(r) != 2 || r[0]["is_error"] != true || r[0]["content"] != "arguments too large (limit 65536 bytes); send less" {
		t.Fatalf("over the limit: %v", r)
	}
	if r[1]["is_error"] == true || !strings.HasPrefix(r[1]["content"].(string), "echo: a") {
		t.Fatalf("at the limit the call runs: %v", r[1])
	}
	calls := e.find(turn, types.EventToolCall)
	marker, _ := calls[0]["arguments"].(map[string]any)
	if marker["truncated"] != true || marker["bytes"] != float64(limit+1) {
		t.Fatalf("stored arguments: %v", calls[0]["arguments"])
	}
	if body := fmt.Sprint(e.fake.Reqs()[1]["messages"]); len(body) > limit {
		t.Fatalf("the next prompt carries %d bytes of the refused call", len(body))
	}
}

// Arguments the event log cannot hold (a NUL, an unpaired surrogate escape:
// Postgres refuses both in jsonb) are answered as invalid, and the turn goes on.
func TestUnstorableArgumentsDoNotWedgeTheTurn(t *testing.T) {
	for _, args := range []string{`{"text":"a\u0000b"}`, `{"text":"a\ud800b"}`} {
		t.Run(args, func(t *testing.T) {
			e := setup(t, func(c *config.Config) { c.Agent.MCP = []string{"files"} },
				fakeprov.Step{Calls: []fakeprov.Call{{ID: "n", Name: "files__echo", Args: args}}},
				fakeprov.Step{Content: "done"})
			turn := e.post("go")
			e.waitState(turn, types.StateCompleted)
			if r := e.find(turn, types.EventToolResult); len(r) != 1 || r[0]["is_error"] != true {
				t.Fatalf("results: %v\n%s", r, e.dump(turn))
			}
		})
	}
}

// Numbers Postgres would expand or refuse are answered as invalid, and the
// turn goes on; a small exponent is plain text and runs.
func TestExponentArgumentsAreRefused(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Agent.MCP = []string{"files"} },
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "huge", Name: "files__echo", Args: `{"text":"x","n":1e131072}`}}},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "spread", Name: "files__echo", Args: `{"text":"x","n":[1e100000]}`}}},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "small", Name: "files__echo", Args: `{"text":"x","n":1E+5}`}}},
		fakeprov.Step{Content: "done"})
	turn := e.post("go")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 3 {
		t.Fatalf("results: %v\n%s", r, e.dump(turn))
	}
	for _, i := range []int{0, 1} {
		if r[i]["is_error"] != true || !strings.HasPrefix(r[i]["content"].(string), "invalid arguments: number out of range") {
			t.Fatalf("result %d: %v", i, r[i])
		}
	}
	// The echo schema has no number field, so the server refuses the extra
	// property; what matters is that the loop let the call through to it.
	if c, _ := r[2]["content"].(string); strings.HasPrefix(c, "invalid arguments") {
		t.Fatalf("a small exponent was refused as invalid: %v", r[2])
	}
}

// A tool name or call id the log cannot hold (NUL, too long) does not wedge the
// turn: a bad name is an unknown tool, stored cleaned and cut; a bad id is
// replaced by the generated one. (The provider drops a call with no name.)
func TestUnstorableNamesAndIDsDoNotWedge(t *testing.T) {
	long := strings.Repeat("n", 300)
	for _, c := range []struct{ name, id, wantName, wantID string }{
		{name: "files__echo\x00", id: "a", wantName: "files__echo", wantID: "a"},
		{name: long, id: "b", wantName: long[:256], wantID: "b"},
		{name: "\x00", id: "c", wantName: "(unnamed)", wantID: "c"},
		{name: "files__echo", id: "d\x00", wantName: "files.echo", wantID: "call_1_0"},
		{name: "files__echo", id: long, wantName: "files.echo", wantID: "call_1_0"},
	} {
		t.Run(c.wantName+" "+c.wantID, func(t *testing.T) {
			e := setup(t, func(cfg *config.Config) { cfg.Agent.MCP = []string{"files"} },
				fakeprov.Step{Calls: []fakeprov.Call{{ID: c.id, Name: c.name, Args: `{"text":"x"}`}}},
				fakeprov.Step{Content: "done"})
			turn := e.post("go")
			e.waitState(turn, types.StateCompleted)
			calls := e.find(turn, types.EventToolCall)
			if len(calls) != 1 || calls[0]["name"] != c.wantName || calls[0]["call_id"] != c.wantID {
				t.Fatalf("recorded call: %v\n%s", calls, e.dump(turn))
			}
			r := e.find(turn, types.EventToolResult)
			if len(r) != 1 || r[0]["call_id"] != c.wantID {
				t.Fatalf("results: %v", r)
			}
		})
	}
}

// Text the model writes is stored cleaned: a NUL in its reply or its reasoning
// is dropped, and the turn completes.
func TestNULInModelTextAndReasoningIsStoredClean(t *testing.T) {
	e := setup(t, nil, fakeprov.Step{Reasoning: "think\x00", Content: "a\x00b"})
	turn := e.post("go")
	e.waitState(turn, types.StateCompleted)
	msgs := e.find(turn, types.EventMessageText)
	if len(msgs) != 2 || msgs[1]["text"] != "ab" {
		t.Fatalf("messages: %v\n%s", msgs, e.dump(turn))
	}
	reasoning := e.find(turn, types.EventReasoning)
	if len(reasoning) != 1 || reasoning[0]["text"] != "think" {
		t.Fatalf("reasoning: %v", reasoning)
	}
}

// A turn that already holds nearly maxTurnEvents (5000) events ends failed in
// the middle of a reply that asks for more calls: the cap is checked before
// each call, so the calls past it are never recorded.
func TestTurnEventsAreCapped(t *testing.T) {
	var calls []fakeprov.Call
	for i := range 10 {
		calls = append(calls, fakeprov.Call{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("nope__%d", i), Args: `{}`})
	}
	e := setup(t, func(c *config.Config) {
		c.Agent.Limits.Steps = 10 // only the events cap may end the turn
		c.Queue.Workers = 0       // this test runs the turn itself
	}, fakeprov.Step{Calls: calls}, fakeprov.Step{Content: "done"})
	turn := e.post("go")
	// Bulk-insert the history: model.usage events are durable but not prompt
	// context. They belong to another agent, so they are not the main agent's steps.
	other, _ := e.store.FindOrCreateActor(e.ctx, e.store.Pool, types.ActorAgent, "summarizer")
	if _, err := e.store.Pool.Exec(e.ctx, `INSERT INTO conversation_events (conversation_id, parent_id, actor_id, scope, type, data, v)
		SELECT $1, $2, $3, 'u1', 'model.usage', '{}', $4 FROM generate_series(1, 4990)`,
		e.conv.ID, turn, other, int(types.Version(types.EventModelUsage))); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if e.state(turn) != types.StateFailed {
		t.Fatalf("state %s\n%s", e.state(turn), e.dump(turn))
	}
	states := e.find(turn, types.EventTurnState)
	if r := states[len(states)-1]["reason"].(string); !strings.Contains(r, "5000 events in one turn") {
		t.Fatalf("reason: %s", r)
	}
	if n := len(e.find(turn, types.EventToolCall)); n == 0 || n >= len(calls) {
		t.Fatalf("recorded calls: %d of %d", n, len(calls))
	}
	if n := len(e.events(turn)); n > 5000+3 {
		t.Fatalf("turn holds %d events", n)
	}
	if n := len(e.fake.Reqs()); n != 1 {
		t.Fatalf("model calls: %d", n)
	}
}

// Without a summarizer, a prompt over the byte cap fails the turn before the
// request is sent.
func TestPromptLimitFailsTheTurnCleanly(t *testing.T) {
	e := setup(t, nil, fakeprov.Step{Content: "first answer"})
	t1 := e.post("first")
	e.waitState(t1, types.StateCompleted)
	e.addMessage(t1, strings.Repeat("a", 9<<20))
	t2 := e.post("second")
	e.waitState(t2, types.StateFailed)
	states := e.find(t2, types.EventTurnState)
	if r := states[len(states)-1]["reason"].(string); !strings.Contains(r, "prompt of") {
		t.Fatalf("reason: %s", r)
	}
	if n := len(e.fake.Reqs()); n != 1 {
		t.Fatalf("model calls: %d, want only the first turn's", n)
	}
}

// Over the byte cap, compaction runs first, even when the token threshold
// does not fire: the summary brings the prompt under the cap and the turn goes
// on. The summarizer's input stays within the cap too.
func TestPromptLimitCompactsBeforeFailing(t *testing.T) {
	// 9,000 lines of 1,000 bytes: over the cap, and split into three summarizer chunks.
	huge := strings.Repeat(strings.Repeat("a", 999)+"\n", 9000)
	e := setup(t, func(c *config.Config) {
		c.Agent.Settings.ContextWindow = 1 << 30 // only the byte cap can trigger compaction
		c.Summarizer = &config.Summarizer{Model: "fake/m", Settings: config.Settings{ContextWindow: 1 << 24}, KeepRecent: 1}
	},
		fakeprov.Step{Content: "reply one"}, fakeprov.Step{Content: "reply two"},
		fakeprov.Step{Content: "part"}, fakeprov.Step{Content: "part"}, fakeprov.Step{Content: "THE SUMMARY"},
		fakeprov.Step{Content: "reply three"})
	t1 := e.post("first")
	e.waitState(t1, types.StateCompleted)
	t2 := e.post("second")
	e.waitState(t2, types.StateCompleted)
	e.addMessage(t1, huge)
	t3 := e.post("third")
	e.waitState(t3, types.StateCompleted)

	if sm := e.find(t3, types.EventSummary); len(sm) != 1 || sm[0]["text"] != "THE SUMMARY" {
		t.Fatalf("summary events: %v\n%s", sm, e.dump(t3))
	}
	reqs := e.fake.Reqs()
	if len(reqs) != 6 {
		t.Fatalf("model calls: %d, want 6 (2 turns, 3 summarizer chunks, the reply)", len(reqs))
	}
	if body := fmt.Sprint(reqs[5]["messages"]); len(body) > 1<<20 {
		t.Fatalf("the reply's prompt still carries the old turn: %d bytes", len(body))
	}
}

// When the summary fails, the turn fails cleanly at the cap: it does not send.
func TestPromptLimitWhenCompactionFails(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.Settings.ContextWindow = 1 << 30
		c.Summarizer = &config.Summarizer{Model: "fake/m", Settings: config.Settings{ContextWindow: 1 << 24}, KeepRecent: 1}
	}, fakeprov.Step{Content: "reply one"}, fakeprov.Step{Content: "reply two"}, fakeprov.Step{Status: 400})
	t1 := e.post("first")
	e.waitState(t1, types.StateCompleted)
	t2 := e.post("second")
	e.waitState(t2, types.StateCompleted)
	e.addMessage(t1, strings.Repeat("a", 9<<20))
	t3 := e.post("third")
	e.waitState(t3, types.StateFailed)
	states := e.find(t3, types.EventTurnState)
	if r := states[len(states)-1]["reason"].(string); !strings.Contains(r, "prompt of") {
		t.Fatalf("reason: %s", r)
	}
	if n := len(e.fake.Reqs()); n != 3 {
		t.Fatalf("model calls: %d, want two replies and the failed summary", n)
	}
}

// A sub-agent's oversize call is refused like the main agent's, and sub-agent
// calls are not stored in the turn.
func TestSubagentArgumentsAreBounded(t *testing.T) {
	const limit = 64 << 10
	pad := func(n int) string { return `{"text":"` + strings.Repeat("a", n-len(`{"text":""}`)) + `"}` }
	e := setup(t, func(c *config.Config) {
		c.Subagents = map[string]config.Subagent{"helper": {Description: "helps", Model: "fake/m",
			Settings: config.Settings{ContextWindow: 100000}}}
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "h", Name: "hfy__helper", Args: `{"task":"go"}`}}},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "big", Name: "files__echo", Args: pad(limit + 1)}}},
		fakeprov.Step{Content: "sub done"},
		fakeprov.Step{Content: "final"})
	turn := e.post("go")
	e.waitState(turn, types.StateCompleted)
	if body := fmt.Sprint(e.fake.Reqs()[2]["messages"]); !strings.Contains(body, "arguments too large") {
		t.Fatalf("the sub-agent was not told: %s", body)
	}
	if calls := e.find(turn, types.EventToolCall); len(calls) != 1 || calls[0]["name"] != "hfy.helper" {
		t.Fatalf("the turn stores only its own calls: %v", calls)
	}
}

// A sub-agent's model steps are the turn's events too: the cap ends the
// sub-agent's run, and the parent's call fails with it.
func TestSubagentStepsCountTowardTheTurnCap(t *testing.T) {
	steps := []fakeprov.Step{{Calls: []fakeprov.Call{{ID: "h", Name: "hfy__helper", Args: `{"task":"go"}`}}}}
	for i := range 8 {
		steps = append(steps, fakeprov.Step{Calls: []fakeprov.Call{{ID: fmt.Sprintf("s%d", i), Name: fmt.Sprintf("nope__%d", i), Args: `{}`}}})
	}
	steps = append(steps, fakeprov.Step{Content: "sub done"}, fakeprov.Step{Content: "final"})
	e := setup(t, func(c *config.Config) {
		c.Queue.Workers = 0 // this test runs the turn itself
		c.Subagents = map[string]config.Subagent{"helper": {Description: "helps", Model: "fake/m",
			Settings: config.Settings{ContextWindow: 100000}}}
	}, steps...)
	turn := e.post("go")
	other, _ := e.store.FindOrCreateActor(e.ctx, e.store.Pool, types.ActorAgent, "summarizer")
	if _, err := e.store.Pool.Exec(e.ctx, `INSERT INTO conversation_events (conversation_id, parent_id, actor_id, scope, type, data, v)
		SELECT $1, $2, $3, 'u1', 'model.usage', '{}', $4 FROM generate_series(1, 4990)`,
		e.conv.ID, turn, other, int(types.Version(types.EventModelUsage))); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if e.state(turn) != types.StateFailed {
		t.Fatalf("state %s\n%s", e.state(turn), e.dump(turn))
	}
	if r := e.find(turn, types.EventToolResult); len(r) != 1 || !strings.Contains(fmt.Sprint(r[0]["content"]), "events in one turn") {
		t.Fatalf("the sub-agent's run must end at the cap: %v", r)
	}
	if n := len(e.fake.Reqs()); n >= len(steps) {
		t.Fatalf("model calls: %d, the sub-agent ran on past the cap", n)
	}
}

// A long turn of ordinary calls stays far below every bound.
func TestLongTurnIsUnaffected(t *testing.T) {
	var steps []fakeprov.Step
	for i := range 25 {
		steps = append(steps, fakeprov.Step{Calls: []fakeprov.Call{
			{ID: fmt.Sprintf("a%d", i), Name: "files__echo", Args: fmt.Sprintf(`{"text":"a%d"}`, i)},
			{ID: fmt.Sprintf("b%d", i), Name: "files__echo", Args: fmt.Sprintf(`{"text":"b%d"}`, i)},
		}})
	}
	steps = append(steps, fakeprov.Step{Content: "done"})
	e := setup(t, func(c *config.Config) { c.Agent.MCP = []string{"files"} }, steps...)
	turn := e.post("go")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 50 {
		t.Fatalf("results: %d", len(r))
	}
	for _, res := range r {
		if res["is_error"] == true {
			t.Fatalf("error result in a normal turn: %v", res)
		}
	}
}
