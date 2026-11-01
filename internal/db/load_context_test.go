package db_test

import (
	"context"
	"testing"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/types"
)

// LoadContext files a note under the turn that was newest when it was
// appended, whatever turn it names. So a note appended late but naming an old
// turn is loaded with the turns after a summary (it is not "covered"), and a
// note appended while an old turn was the newest is covered with it.
func TestLoadContextFilesNotesByEventOrder(t *testing.T) {
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
	early := add(&t1.ID, types.ContextNote{Text: "early"}) // t1 is the newest turn
	t2 := add(nil, types.Turn{})
	add(&t2.ID, types.MessageText{Text: "hi"})
	late := add(&t1.ID, types.HostMessage{Text: "late"}) // names turn 1, appended after turn 2 began
	t3 := add(nil, types.Turn{})
	later := add(&t1.ID, types.ContextNote{Text: "later"}) // names turn 1, appended after turn 3 began

	// A summary covering turn 1 leaves turns 2 and 3 and everything appended during them.
	rows, err := s.LoadContext(ctx, conv.ID, t1.ID, t3.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, r := range rows {
		got = append(got, r.ID)
		if r.Type == types.EventContextNote || r.Type == types.EventHostMessage {
			if r.ParentID == nil || *r.ParentID == t1.ID {
				t.Errorf("note %d is still filed under turn 1", r.ID)
			}
		}
	}
	msg := rows[1].ID // the "hi" message
	want := []int64{t2.ID, msg, late.ID, t3.ID, later.ID}
	if len(got) != len(want) {
		t.Fatalf("loaded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("loaded %v, want %v", got, want)
		}
	}
	if *rows[2].ParentID != t2.ID || *rows[4].ParentID != t3.ID {
		t.Fatalf("filed under %d and %d, want turns %d and %d", *rows[2].ParentID, *rows[4].ParentID, t2.ID, t3.ID)
	}

	// Without a summary, everything is there, the note named for an old turn in its event place.
	rows, err = s.LoadContext(ctx, conv.ID, 0, t3.ID)
	if err != nil {
		t.Fatal(err)
	}
	var all []int64
	for _, r := range rows {
		all = append(all, r.ID)
	}
	if len(all) != 7 || all[1] != early.ID {
		t.Fatalf("without a summary: %v", all)
	}
	// Turn 2 running: it does not see what was appended after it (turn 3's notes).
	rows, _ = s.LoadContext(ctx, conv.ID, t1.ID, t2.ID)
	for _, r := range rows {
		if r.ID == later.ID {
			t.Fatal("a note appended after turn 3 began is not part of turn 2's context")
		}
	}
}
