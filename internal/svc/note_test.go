package svc_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

func noteFixture(t *testing.T) (context.Context, *db.Store, *svc.Service, db.ConversationRow, int64) {
	t.Helper()
	ctx := context.Background()
	store := dbtest.New(t)
	s := svc.New(store, &config.Config{Queue: config.Queue{MaxAttempts: 3}})
	conv, err := store.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := store.FindOrCreateActor(ctx, store.Pool, types.ActorExternal, "u1")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, s, conv, actor
}

func countJobs(t *testing.T, store *db.Store) (n int) {
	t.Helper()
	if err := store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A note goes through the one event choke point, under the newest turn when
// none is running, with its scope, in any turn state, and never starts or
// queues a turn.
func TestAppendNoteUnderTheNewestTurn(t *testing.T) {
	ctx, store, s, conv, actor := noteFixture(t)
	in := svc.NoteInput{ConversationID: conv.ID, Actor: actor, Type: types.EventContextNote, Text: "plan: pro"}
	if _, err := s.AppendNote(ctx, in); !errors.Is(err, svc.ErrNoTurn) {
		t.Fatalf("no turn yet: %v", err)
	}

	t1, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: actor, Scope: "scope-a", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	jobs := countJobs(t, store) // the run_turn of the post: notes add none
	t1ID, _ := store.TurnIDByUUID(ctx, conv.ID, t1)
	// while the turn is queued (not even started)
	n1, err := s.AppendNote(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if n1.Type != types.EventContextNote || n1.ParentID == nil || *n1.ParentID != t1ID || n1.Scope != "scope-a" || n1.ActorID != actor {
		t.Fatalf("note row: %+v", n1)
	}
	if err := s.EndTurn(ctx, conv.ID, t1ID, types.TurnStateEvent{State: types.StateCompleted}); err != nil {
		t.Fatal(err)
	}
	in.Type, in.Text = types.EventHostMessage, "\uFEFFpayment\x00 received"
	// after it ended
	n2, err := s.AppendNote(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := n2.Payload[types.HostMessage](); err != nil || p.Text != "payment received" {
		t.Fatalf("cleaned payload: %+v %v", p, err)
	}
	t2, _ := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: actor, Scope: "scope-b", Text: "again"})
	t2ID, _ := store.TurnIDByUUID(ctx, conv.ID, t2)
	n3, err := s.AppendNote(ctx, svc.NoteInput{ConversationID: conv.ID, Actor: actor, Type: types.EventContextNote, Text: "later"})
	if err != nil || *n3.ParentID != t2ID || n3.Scope != "scope-b" {
		t.Fatalf("a note goes under the newest turn: %+v %v", n3, err)
	}
	if got := countJobs(t, store); got != jobs {
		t.Fatalf("notes must not enqueue jobs: %d -> %d", jobs, got)
	}

	// Durable: history shows it under its turn, the stream shows it by id.
	hist, err := store.HistoryEvents(ctx, conv.ID, []int64{t1ID})
	if err != nil {
		t.Fatal(err)
	}
	var seen []types.EventType
	for _, e := range hist {
		seen = append(seen, e.Type)
	}
	if !slices.Contains(seen, types.EventContextNote) || !slices.Contains(seen, types.EventHostMessage) {
		t.Fatalf("history of turn 1: %v", seen)
	}
	after, err := store.EventsAfter(ctx, conv.ID, n1.ID-1, 1)
	if err != nil || len(after) != 1 || after[0].ID != n1.ID {
		t.Fatalf("stream: %v %v", after, err)
	}
}

// A note is for the turn that is running: a turn queued behind it must not
// take the note from it, and once the running turn ends the note goes to the
// newest one.
func TestAppendNoteGoesToTheRunningTurn(t *testing.T) {
	ctx, store, s, conv, actor := noteFixture(t)
	post := func(text string) int64 {
		t.Helper()
		u, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: actor, Scope: "u1", Text: text})
		if err != nil {
			t.Fatal(err)
		}
		id, err := store.TurnIDByUUID(ctx, conv.ID, u)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := post("A")
	if ok, err := s.StartTurn(ctx, conv.ID, a); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	b := post("B") // queued behind the running A
	note := func(text string) db.EventRow {
		t.Helper()
		n, err := s.AppendNote(ctx, svc.NoteInput{ConversationID: conv.ID, Actor: actor, Type: types.EventContextNote, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := note("for A"); *n.ParentID != a {
		t.Fatalf("a note appended while A runs is filed under turn %d, want A (%d), not the queued B (%d)", *n.ParentID, a, b)
	}
	if err := s.EndTurn(ctx, conv.ID, a, types.TurnStateEvent{State: types.StateCompleted}); err != nil {
		t.Fatal(err)
	}
	if n := note("for B"); *n.ParentID != b {
		t.Fatalf("with nothing running the note goes to the newest turn %d, got %d", b, *n.ParentID)
	}
}

// With nothing running and two turns queued, the note is for the one that
// runs first, not the newest: filed under the newer, it would reach the first
// turn a turn late.
func TestAppendNoteGoesToTheFirstQueuedTurn(t *testing.T) {
	ctx, store, s, conv, actor := noteFixture(t)
	var ids []int64
	for _, text := range []string{"A", "B"} {
		u, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: actor, Scope: "u1", Text: text})
		if err != nil {
			t.Fatal(err)
		}
		id, _ := store.TurnIDByUUID(ctx, conv.ID, u)
		ids = append(ids, id)
	}
	n, err := s.AppendNote(ctx, svc.NoteInput{ConversationID: conv.ID, Actor: actor, Type: types.EventContextNote, Text: "for A"})
	if err != nil {
		t.Fatal(err)
	}
	if *n.ParentID != ids[0] {
		t.Fatalf("filed under turn %d, want the first queued turn %d (not %d)", *n.ParentID, ids[0], ids[1])
	}
}

// What is not a clean, bounded, non-blank note is a 400, whatever the type.
func TestAppendNoteRefusesBadText(t *testing.T) {
	ctx, store, s, conv, actor := noteFixture(t)
	if _, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: actor, Scope: "u1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	var before int
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_events`).Scan(&before)
	bad := map[string]svc.NoteInput{
		"blank":     {Type: types.EventContextNote, Text: "  \n"},
		"empty":     {Type: types.EventHostMessage, Text: ""},
		"NUL only":  {Type: types.EventContextNote, Text: "\x00"},
		"too big":   {Type: types.EventContextNote, Text: strings.Repeat("a", types.MaxNoteBytes+1)},
		"bad type":  {Type: types.EventMessageText, Text: "x"},
		"turn type": {Type: types.EventTurn, Text: "x"},
	}
	for name, in := range bad {
		in.ConversationID, in.Actor = conv.ID, actor
		var br *svc.BadRequestError
		if _, err := s.AppendNote(ctx, in); !errors.As(err, &br) {
			t.Errorf("%s: want a BadRequestError, got %v", name, err)
		}
	}
	var after int
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_events`).Scan(&after)
	if after != before {
		t.Fatalf("refused notes wrote %d events", after-before)
	}
	// the bound is on the cleaned text, and is exact
	ok := svc.NoteInput{ConversationID: conv.ID, Actor: actor, Type: types.EventContextNote,
		Text: strings.Repeat("a", types.MaxNoteBytes) + "\x00"}
	if _, err := s.AppendNote(ctx, ok); err != nil {
		t.Fatalf("a note at the bound once cleaned: %v", err)
	}
}

// The worst case on the way back: every byte a 6-byte escape in the stored
// JSON. It must round-trip intact and stay within 6x the bound.
func TestNoteRoundTripsWorstCaseEscapes(t *testing.T) {
	ctx, store, s, conv, actor := noteFixture(t)
	if _, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: actor, Scope: "u1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("<\x01", types.MaxNoteBytes/2)
	row, err := s.AppendNote(ctx, svc.NoteInput{ConversationID: conv.ID, Actor: actor, Type: types.EventContextNote, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	back, err := store.EventByID(ctx, store.Pool, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := back.Payload[types.ContextNote]()
	if err != nil || p.Text != text {
		t.Fatalf("round trip changed the text: %v", err)
	}
	if len(back.Data) > 6*types.MaxNoteBytes+64 {
		t.Fatalf("stored payload is %d bytes", len(back.Data))
	}
}
