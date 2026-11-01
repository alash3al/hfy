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

// Postgres is the judge of the number rule. A form storable accepts must store
// and read back within the argument bound. The forms it refuses must fail to
// store, or read back past the bound.
func TestStorableAgreesWithPostgres(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New(t, nil)
	ones := func(n int) string { return "1" + strings.Repeat("0", n-1) }
	for _, c := range []struct{ raw, pg string }{
		{`[1e131072]`, "fails"},
		{`[1e100000]`, "bloats"},
		{`{"x":1e-16384}`, "fails"},
		{`[1e131071]`, "bloats"},
		{`[0.` + strings.Repeat("0", 16383) + `1]`, "fails"},
		{`{"x":"\u0000"}`, "fails"},
		{`{"x":"\ud800"}`, "fails"},
		{`{"x":"\ud83d\ude00"}`, "stores"},
		{`{"x":1E+5}`, "stores"},
		{`{"x":1.5e-3}`, "stores"},
		{`{"x":-0}`, "stores"},
		{`{"x":` + ones(1000) + `}`, "stores"},
	} {
		var text string
		outcome := "stores"
		switch err := store.Pool.QueryRow(ctx, "SELECT CAST($1::text AS jsonb)::text", c.raw).Scan(&text); {
		case err != nil:
			outcome = "fails"
		case len(text) > maxArgBytes:
			outcome = "bloats"
		}
		if outcome != c.pg {
			t.Errorf("postgres %s %.40q: got %s, want %s", c.raw, c.raw, outcome, c.pg)
		}
		if ours := storable(c.raw, maxArgBytes) == nil; ours != (c.pg == "stores") {
			t.Errorf("storable(%.40q) = %v, postgres says %s", c.raw, ours, c.pg)
		}
	}
}
