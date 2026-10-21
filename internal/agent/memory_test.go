package agent_test

import (
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
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
	// The summarizer read a framed transcript (an anonymous person has no from).
	input := reqs[2]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(input, `<message kind="person">first `) || !strings.Contains(input, `<message from="main" kind="agent">reply one</message>`) {
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

func TestConversationSearchIsScopedAndFindsMemory(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Embeddings = &config.Embeddings{Model: "fake/e", Dimensions: 3}
	},
		fakeprov.Step{Content: "noted"},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "s", Name: "hfy__conversation_search", Args: `{"query":"fruit apples"}`}}},
		fakeprov.Step{Content: "you like apples"},
	)
	t1 := e.post("my favorite fruit is apples")
	e.waitState(t1, types.StateCompleted)
	waitFor(t, func() bool {
		var n int
		e.store.Pool.QueryRow(e.ctx, "SELECT count(*) FROM embeddings_3").Scan(&n)
		if n < 2 { // user message and reply
			rows, _ := e.store.Pool.Query(e.ctx, "SELECT type, queue, state, attempts, coalesce(last_error,'') FROM jobs")
			for rows.Next() {
				var a, b, c, d string
				var at int
				rows.Scan(&a, &b, &c, &at, &d)
				t.Logf("job %s %s %s attempts=%d err=%s", a, b, c, at, d)
			}
			rows.Close()
		}
		return n >= 2
	})
	// A message from another scope must not be visible.
	other, _ := e.store.FindOrCreateActor(e.ctx, e.store.Pool, types.ActorExternal, "someone-else")
	e.store.Pool.Exec(e.ctx, `INSERT INTO embeddings_3 (embeddable_type, embeddable_id, scope, model, embedding)
		SELECT 'conversation_event', id, 'other-scope', 'e', '[1,0,0]' FROM conversation_events WHERE type='turn' LIMIT 1`)
	_ = other

	t2 := e.post("what do I like?")
	e.waitState(t2, types.StateCompleted)
	r := e.find(t2, types.EventToolResult)
	if len(r) != 1 || !strings.Contains(r[0]["content"].(string), "apples") {
		t.Fatalf("conversation_search result: %v\n%s", r, e.dump(t2))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestEmbeddingsTargetedBackfillAndDimensionMismatch(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Embeddings = &config.Embeddings{Model: "fake/e", Dimensions: 3}
	}, fakeprov.Step{Content: "the reply gets a vector too"})
	count := func() int {
		var n int
		e.store.Pool.QueryRow(e.ctx, "SELECT count(*) FROM embeddings_3").Scan(&n)
		return n
	}
	// Targeted jobs: the user message (from Post) and the agent reply (from the loop).
	turn := e.post("hello there")
	e.waitState(turn, types.StateCompleted)
	waitFor(t, func() bool { return count() == 2 })

	// Backfill: a message written without a job is picked up on the next boot.
	main, _ := e.store.FindOrCreateActor(e.ctx, e.store.Pool, types.ActorAgent, "main")
	e.store.AppendEvent(e.ctx, e.store.Pool, db.NewEvent{ConversationID: e.conv.ID, ParentID: &turn, ActorID: main,
		Scope: types.NoScope, Payload: types.MessageText{Text: "written behind the queue's back"}})
	if err := e.rt.Boot(e.ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return count() == 3 })

	// A model whose vectors do not match embeddings.dimensions fails the job loudly.
	e.fake.EmbedDim = 4
	e.post("this one cannot be stored")
	waitFor(t, func() bool {
		var msg string
		e.store.Pool.QueryRow(e.ctx, `SELECT coalesce(last_error, '') FROM jobs WHERE type = 'embed_events'
			AND last_error IS NOT NULL ORDER BY id DESC LIMIT 1`).Scan(&msg)
		return strings.Contains(msg, "returned 4 dimensions, embeddings.dimensions is 3")
	})
}
