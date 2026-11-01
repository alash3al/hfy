package agent_test

import (
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/types"
)

func TestSummarizerFoldsOldTurns(t *testing.T) {
	long := strings.Repeat("word ", 80) // ~400 chars, ~100 tokens
	e := setup(t, func(c *config.Config) {
		c.Agent.Settings.ContextWindow = 150
		c.Summarizer = &config.Summarizer{Model: "fake/m", Settings: config.Settings{ContextWindow: 4000}, KeepRecent: 1}
	},
		fakeprov.Step{Content: "reply one"},
		fakeprov.Step{Content: "reply two"},
		fakeprov.Step{Content: "THE SUMMARY"}, // the summarizer call at the start of turn 3
		fakeprov.Step{Content: "reply three"},
	)
	t1 := e.post("first " + long)
	e.waitState(t1, types.StateCompleted)
	t2 := e.post("second " + long)
	e.waitState(t2, types.StateCompleted)
	t3 := e.post("third " + long)
	e.waitState(t3, types.StateCompleted)

	sm := e.find(t3, types.EventSummary)
	if len(sm) != 1 || sm[0]["text"] != "THE SUMMARY" {
		t.Fatalf("summary events: %v\n%s", sm, e.dump(t3))
	}
	s, _ := e.store.ConversationByID(e.ctx, e.conv.ID)
	if s.SummaryID == nil {
		t.Fatal("conversations.summary_id not set")
	}
	// Nothing was deleted: turn 1 is still in history.
	if len(e.find(t1, types.EventMessageText)) != 2 {
		t.Fatal("history must stay intact")
	}
	reqs := e.fake.Reqs()
	// The summarizer read a framed transcript.
	input := reqs[2]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(input, `<message from="u1" kind="person">first `) || !strings.Contains(input, `<message from="main" kind="agent">reply one</message>`) {
		t.Fatalf("summarizer input:\n%s", input)
	}
	// The last model call saw the summary, turn 2 and 3, but not turn 1.
	last := reqs[len(reqs)-1]["messages"].([]any)
	var all strings.Builder
	for _, m := range last {
		c, _ := m.(map[string]any)["content"].(string)
		all.WriteString(c + "\n")
	}
	body := all.String()
	if !strings.Contains(body, "THE SUMMARY") || !strings.Contains(body, "second") || strings.Contains(body, "first") {
		t.Fatalf("context after summarizing:\n%s", body)
	}
	// The summarizer ran as its own actor.
	var n int
	e.store.Pool.QueryRow(e.ctx, `SELECT count(*) FROM conversation_events e JOIN actors a ON a.id=e.actor_id
		WHERE a.ref='summarizer' AND a.type='agent'`).Scan(&n)
	if n < 2 { // model.usage + summary
		t.Fatalf("summarizer events: %d", n)
	}
}

// A summarizer that fails (a request it rejects, a retired model) costs the
// turn its compaction, not its answer: the turn goes on uncompacted, without
// a second try, and later turns still run.
func TestFailingSummarizerDoesNotFailTheTurn(t *testing.T) {
	long := strings.Repeat("word ", 80)
	e := setup(t, func(c *config.Config) {
		c.Agent.Settings.ContextWindow = 150
		c.Summarizer = &config.Summarizer{Model: "fake/m", Settings: config.Settings{ContextWindow: 4000}, KeepRecent: 1}
	},
		fakeprov.Step{Content: "reply one"},
		fakeprov.Step{Content: "reply two"},
		fakeprov.Step{Status: 400}, // the summarizer at the start of turn 3
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "c", Name: "files__echo", Args: `{"text":"x"}`}}, Prompt: 300},
		fakeprov.Step{Content: "reply three"}, // still over 80%, but no second try in this turn
		fakeprov.Step{Status: 400},            // turn 4 tries again
		fakeprov.Step{Content: "reply four"},
	)
	var turns []int64
	for _, text := range []string{"first ", "second ", "third ", "fourth "} {
		turn := e.post(text + long)
		e.waitState(turn, types.StateCompleted)
		turns = append(turns, turn)
	}
	for _, turn := range turns {
		if sm := e.find(turn, types.EventSummary); len(sm) != 0 {
			t.Fatalf("summary events: %v", sm)
		}
	}
	if n := len(e.fake.Reqs()); n != 7 {
		t.Fatalf("model calls: %d", n)
	}
}

func TestSubagentIsIsolatedAndCounted(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Subagents = map[string]config.Subagent{"refunds": {
			Description: "Decides refund eligibility", Model: "fake/m", System: "You are a refunds specialist.",
		}}
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "r", Name: "hfy__refunds", Args: `{"task":"order 42, delivered late"}`}}, Prompt: 10, Output: 1},
		fakeprov.Step{Content: "eligible for refund", Prompt: 7, Output: 2},
		fakeprov.Step{Content: "Yes, you get a refund.", Prompt: 20, Output: 3},
	)
	turn := e.post("my order 42 was late; secret context")
	e.waitState(turn, types.StateCompleted)

	// The main agent was offered the sub-agent as a tool with its description.
	tools := e.fake.Reqs()[0]["tools"].([]any)
	found := false
	for _, tl := range tools {
		fn := tl.(map[string]any)["function"].(map[string]any)
		if fn["name"] == "hfy__refunds" && fn["description"] == "Decides refund eligibility" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sub-agent tool missing: %v", tools)
	}
	// The sub-agent saw only its system prompt and the task.
	sub := e.fake.Reqs()[1]["messages"].([]any)
	if len(sub) != 2 || sub[1].(map[string]any)["content"] != "order 42, delivered late" ||
		strings.Contains(sub[0].(map[string]any)["content"].(string), "everyone") {
		t.Fatalf("sub-agent request: %v", sub)
	}
	if r := e.find(turn, types.EventToolResult); len(r) != 1 || r[0]["content"] != "eligible for refund" {
		t.Fatalf("tool result: %v", r)
	}
	// Its usage is recorded under its own actor and counts toward the turn.
	states := e.find(turn, types.EventTurnState)
	usage := states[len(states)-1]["usage"].(map[string]any)
	if usage["input"] != float64(37) || usage["output"] != float64(6) {
		t.Fatalf("turn usage: %v", usage)
	}
	var n int
	e.store.Pool.QueryRow(e.ctx, `SELECT count(*) FROM conversation_events e JOIN actors a ON a.id=e.actor_id
		WHERE a.ref='refunds' AND e.type='model.usage'`).Scan(&n)
	if n != 1 {
		t.Fatalf("sub-agent usage events: %d", n)
	}
}
