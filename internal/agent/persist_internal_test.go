package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// persistFixture is a turn in a fresh database, with the runtime and actor
// that write to it.
type persistFixture struct {
	rt    *Runtime
	env   *TurnEnv
	actor int64
	turn  db.EventRow
}

func newPersistFixture(t *testing.T) persistFixture {
	t.Helper()
	ctx := context.Background()
	store := dbtest.New(t, nil)
	cfg := &config.Config{}
	rt := &Runtime{Cfg: cfg, Store: store, Svc: svc.New(store, cfg)}
	conv, err := store.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := store.FindOrCreateActor(ctx, store.Pool, types.ActorExternal, "u1")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendEvent(ctx, store.Pool, db.NewEvent{ConversationID: conv.ID, ActorID: actor,
		Scope: "u1", Payload: types.Turn{Scope: "u1"}})
	if err != nil {
		t.Fatal(err)
	}
	env := &TurnEnv{ConversationID: conv.ID, TurnID: turn.ID, Scope: "u1", Total: &usageAcc{}}
	return persistFixture{rt: rt, env: env, actor: actor, turn: turn}
}

// Streamed deltas are rows of their turn but never count toward maxTurnEvents,
// however many arrive: the live path counts exactly what the log counts.
func TestStreamedDeltasDoNotCountTowardTheTurnCap(t *testing.T) {
	ctx := context.Background()
	f := newPersistFixture(t)
	f.env.events.Store(maxTurnEvents - 3) // three events of headroom
	sink := newTurnSink(ctx, f.rt, f.env, f.actor)
	for range 20 {
		sink.TextDelta("ab")
		sink.flush(ctx)
	}
	sink.ReasoningDelta("r")
	sink.flush(ctx)

	if got := f.env.events.Load(); got != maxTurnEvents-3 {
		t.Fatalf("events counted %d after the deltas, want %d", got, maxTurnEvents-3)
	}
	if why := (&loop{env: f.env}).eventsLimit(); why != "" {
		t.Fatalf("deltas ended the turn: %s", why)
	}
	var n int
	if err := f.rt.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM conversation_events WHERE parent_id = $1 AND type = $2",
		f.turn.ID, string(types.EventTextDelta)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Fatalf("text.delta rows: %d, want 20", n)
	}
}

// A tool's output is stored cleaned: no NUL, and no invalid UTF-8.
func TestToolResultTextIsStoredClean(t *testing.T) {
	ctx := context.Background()
	f := newPersistFixture(t)
	sink := newTurnSink(ctx, f.rt, f.env, f.actor)
	if err := sink.CallFinished(ctx, CallInfo{ID: "c1", Name: "x"}, mcp.Result{Content: "a\x00b\xff"}); err != nil {
		t.Fatal(err)
	}
	events, err := f.rt.Store.TurnEvents(ctx, f.rt.Store.Pool, f.turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type != types.EventToolResult {
			continue
		}
		r, err := e.Payload[types.ToolResult]()
		if err != nil {
			t.Fatal(err)
		}
		if r.Content != "ab\uFFFD" {
			t.Fatalf("stored content %q", r.Content)
		}
		return
	}
	t.Fatal("no tool result stored")
}

// The error that ends an exhausted turn is stored cleaned too.
func TestExhaustedTurnErrorIsStoredClean(t *testing.T) {
	ctx := context.Background()
	f := newPersistFixture(t)
	payload, _ := json.Marshal(svc.RunTurnPayload{TurnEventID: f.turn.ID})
	f.rt.onTurnExhausted(ctx, db.JobRow{Payload: payload, Attempts: 2}, errors.New("boom\x00"))
	st, err := f.rt.Store.TurnStateOf(ctx, f.rt.Store.Pool, f.turn.ID)
	if err != nil || st == nil {
		t.Fatalf("turn state: %v %v", st, err)
	}
	if st.State != types.StateFailed || strings.ContainsRune(st.Error, 0) || !strings.Contains(st.Error, "boom") {
		t.Fatalf("turn state %+v", st)
	}
}

// nested is JSON of n levels: n arrays, or n objects keyed "a".
func nested(n int, objects bool) string {
	if objects {
		return strings.Repeat(`{"a":`, n-1) + `{}` + strings.Repeat(`}`, n-1)
	}
	return strings.Repeat("[", n) + strings.Repeat("]", n)
}

// pgOutcome is what Postgres does with raw as a tool call's arguments: the
// insert fails ("fails"), the value reads back past maxArgBytes ("bloats"),
// the event cannot be read back once the payload wraps it ("unreadable"), or
// it all works ("stores").
func pgOutcome(t *testing.T, f persistFixture, raw string) string {
	t.Helper()
	ctx := context.Background()
	var text string
	if err := f.rt.Store.Pool.QueryRow(ctx, "SELECT CAST($1::text AS jsonb)::text", raw).Scan(&text); err != nil {
		return "fails"
	}
	if len(text) > maxArgBytes {
		return "bloats"
	}
	row, err := f.rt.Store.AppendEvent(ctx, f.rt.Store.Pool, db.NewEvent{ConversationID: f.env.ConversationID,
		ParentID: &f.turn.ID, ActorID: f.actor, Scope: "u1",
		Payload: types.ToolCall{Name: "x", CallID: "c", Arguments: json.RawMessage(raw)}})
	if err != nil {
		return "unreadable"
	}
	if row, err = f.rt.Store.EventByID(ctx, f.rt.Store.Pool, row.ID); err != nil {
		return "unreadable"
	}
	if _, err := row.Payload[types.ToolCall](); err != nil {
		return "unreadable"
	}
	if _, err := svc.ViewOf([]db.EventRow{row}); err != nil {
		return "unreadable"
	}
	return "stores"
}

// Postgres is the judge of the JSON rules. A form storable accepts must store
// and read back, wrapped in its event, within the argument bound. A form it
// refuses must fail to store, read back past the bound, or not read back as an
// event: or be one of the conservative refusals (conservative), which Postgres
// would take but a rule of ours turns away.
func TestStorableAgreesWithPostgres(t *testing.T) {
	f := newPersistFixture(t)
	ones := func(n int) string { return "1" + strings.Repeat("0", n-1) }
	// A separators-only array of k ones reads back as 3k+3 bytes.
	commas := func(k int) string { return "[" + strings.Repeat("1,", k) + "1]" }
	atLimit := (maxArgBytes - 3) / 3
	for _, c := range []struct {
		raw          string
		pg           string
		conservative bool
	}{
		{raw: `[1e131072]`, pg: "fails"},
		{raw: `[1e100000]`, pg: "bloats"},
		{raw: `{"x":1e-16384}`, pg: "fails"},
		{raw: `[1e131071]`, pg: "bloats"},
		{raw: `[0.` + strings.Repeat("0", 16383) + `1]`, pg: "fails"},
		{raw: `{"x":"\u0000"}`, pg: "fails"},
		{raw: `{"x":"\ud800"}`, pg: "fails"},
		{raw: `{"x":"\ud83d\ude00"}`, pg: "stores"},
		{raw: `{"x":1E+5}`, pg: "stores"},
		{raw: `{"x":1.5e-3}`, pg: "stores"},
		{raw: `{"x":-0}`, pg: "stores"},
		{raw: `{"x":` + ones(1000) + `}`, pg: "stores"},
		// Repeated keys: Postgres parses every value and keeps the last.
		{raw: `{"a":"\u0000","a":1}`, pg: "fails"},
		{raw: `{"a":"\ud800","a":1}`, pg: "fails"},
		{raw: `{"a":1e999999,"a":1}`, pg: "fails"},
		{raw: `{"a":1,"a":"\u0000"}`, pg: "fails"},
		{raw: `{"a":{"b":"\u0000"},"a":[1]}`, pg: "fails"},
		{raw: `{"a":1,"a":2}`, pg: "stores"},
		{raw: `{"a":1e100000,"a":1}`, pg: "stores", conservative: true}, // the bomb is dropped, but we cannot tell
		// Keys are text too.
		{raw: `{"\u0000":1}`, pg: "fails"},
		{raw: `{"\ud800":1}`, pg: "fails"},
		{raw: `{"k\u00e9":1,"\ud83d\ude00":2}`, pg: "stores"},
		// Not text, or not JSON.
		{raw: "\ufeff{}", pg: "fails"},
		{raw: "{\"x\":\"\xff\"}", pg: "fails"},
		{raw: "{\"x\":\"a\x00b\"}", pg: "fails"},
		// Depth: the payload wraps the arguments one level deeper.
		{raw: nested(63, false), pg: "stores"},
		{raw: nested(64, false), pg: "stores"},
		{raw: nested(65, false), pg: "stores", conservative: true},
		{raw: nested(63, true), pg: "stores"},
		{raw: nested(64, true), pg: "stores"},
		{raw: nested(65, true), pg: "stores", conservative: true},
		{raw: nested(9999, false), pg: "stores", conservative: true},
		{raw: nested(10000, false), pg: "unreadable"},
		{raw: nested(10000, true), pg: "bloats"}, // "a": reads back 7 bytes a level
		// Separators: Postgres prints a space after each "," and ":".
		{raw: commas(atLimit), pg: "stores"},
		{raw: commas(atLimit + 1), pg: "bloats"},
		{raw: `{"a":` + commas(atLimit-2) + `}`, pg: "stores"}, // 7 bytes more: the key's wrapper
		{raw: `{"a":` + commas(atLimit-1) + `}`, pg: "bloats"},
		// Common values pass.
		{raw: `{"n":3.14159,"e":1e10,"big":12345678901234567890,"neg":-1.5E-7}`, pg: "stores"},
		{raw: `{"max":1.7976931348623157e308,"min":-1.7976931348623157e308,"tiny":5e-324,"norm":2.2250738585072014e-308}`, pg: "stores"},
		{raw: `{"text":"héllo wörld 日本語 \ud83d\ude00 \u00e9 \n\t\"","empty":"","list":[],"obj":{}}`, pg: "stores"},
		{raw: ` { "a" : [ 1 , 2 , { "b" : null } ] } `, pg: "stores"},
		{raw: `{}`, pg: "stores"},
		// Valid but refused: a number that long written out.
		{raw: `[` + ones(1001) + `]`, pg: "stores", conservative: true},
		{raw: `[0e1000]`, pg: "stores", conservative: true},
		{raw: `[-0.0e-999]`, pg: "stores", conservative: true},
	} {
		got := pgOutcome(t, f, c.raw)
		if got != c.pg {
			t.Errorf("postgres %.40q: got %s, want %s", c.raw, got, c.pg)
		}
		want := c.pg == "stores" && !c.conservative
		if ours := storable(c.raw, maxArgBytes) == nil; ours != want {
			t.Errorf("storable(%.40q) = %v, want %v (postgres says %s)", c.raw, ours, want, c.pg)
		}
	}
}

// Whatever storable accepts, with its structured-content wrappers, stores and
// reads back: the deepest value sits one level down in a tool_result's
// structured and ui, and two in an approval request's held result.
func TestStorableDepthFitsEveryWrapper(t *testing.T) {
	ctx := context.Background()
	f := newPersistFixture(t)
	deep := json.RawMessage(nested(maxDepth, true))
	if storable(string(deep), maxStructured) != nil || storable(nested(maxDepth+1, true), maxStructured) == nil {
		t.Fatal("the bound is not maxDepth")
	}
	for name, p := range map[string]types.Payload{
		"tool_call":  types.ToolCall{Name: "x", CallID: "c", Arguments: deep},
		"structured": types.ToolResult{CallID: "c", Structured: deep, UI: deep},
		"held":       types.ApprovalRequested{CallID: "c", Questions: deep, UI: deep, Held: &types.HeldResult{Structured: deep}},
	} {
		row, err := f.rt.Store.AppendEvent(ctx, f.rt.Store.Pool, db.NewEvent{ConversationID: f.env.ConversationID,
			ParentID: &f.turn.ID, ActorID: f.actor, Scope: "u1", Payload: p})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		events, err := f.rt.Store.TurnEvents(ctx, f.rt.Store.Pool, f.turn.ID)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := svc.ViewOf(events); err != nil {
			t.Fatalf("%s: read back: %v", name, err)
		}
		if _, err := f.rt.Store.EventByID(ctx, f.rt.Store.Pool, row.ID); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
