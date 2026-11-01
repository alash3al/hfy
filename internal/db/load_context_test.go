package db_test

import (
	"context"
	"slices"
	"testing"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/types"
)

// LoadContext returns notes and host messages with the turn they are filed
// under, in event order, like every other event: a summary that covers a turn
// leaves out its notes (the summarizer folded them) and a turn's context
// stops at that turn.
func TestLoadContextLoadsNotesWithTheirTurn(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := s.FindOrCreateActor(ctx, s.Pool, types.ActorExternal, "u1")
	add := func(parent *int64, p types.Payload) db.EventRow {
		t.Helper()
		e, err := s.AppendEvent(ctx, s.Pool, db.NewEvent{ConversationID: conv.ID, ParentID: parent, ActorID: actor, Scope: "u1", Payload: p})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	t1 := add(nil, types.Turn{})
	n1 := add(&t1.ID, types.ContextNote{Text: "one"})
	t2 := add(nil, types.Turn{})
	m2 := add(&t2.ID, types.MessageText{Text: "hi"})
	n2 := add(&t2.ID, types.HostMessage{Text: "two"})
	t3 := add(nil, types.Turn{})
	n3 := add(&t3.ID, types.ContextNote{Text: "three"})
	// a note for turn 2 appended after turn 3 was queued still belongs to turn 2
	late := add(&t2.ID, types.ContextNote{Text: "late for two"})

	ids := func(after, upto int64) []int64 {
		rows, err := s.LoadContext(ctx, conv.ID, after, upto)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	for _, c := range []struct {
		name        string
		after, upto int64
		want        []int64
	}{
		{"everything", 0, t3.ID, []int64{t1.ID, n1.ID, t2.ID, m2.ID, n2.ID, late.ID, t3.ID, n3.ID}},
		{"after a summary covering turn 1", t1.ID, t3.ID, []int64{t2.ID, m2.ID, n2.ID, late.ID, t3.ID, n3.ID}},
		{"turn 2 running", t1.ID, t2.ID, []int64{t2.ID, m2.ID, n2.ID, late.ID}},
	} {
		if got := ids(c.after, c.upto); !slices.Equal(got, c.want) {
			t.Errorf("%s: loaded %v, want %v", c.name, got, c.want)
		}
	}
}
