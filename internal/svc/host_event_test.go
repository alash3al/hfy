package svc_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

func hostEvent(conv db.ConversationRow, host int64, key, text string) svc.NoteInput {
	return svc.NoteInput{ConversationID: conv.ID, Actor: host, Type: types.EventContextNote, Text: text, Key: key, Limit: svc.MaxHostEventsPerTurn}
}

// A keyed note is stored once per (conversation, actor, key): a repeat gives
// back the first event whatever it says, even of another type, even after the
// turn it was filed under has ended and a newer one exists. Another actor, or
// another conversation, has its own keys; keys are case-sensitive.
func TestAppendNoteOnceReplaysTheFirstEvent(t *testing.T) {
	ctx, store, s, conv, user := noteFixture(t)
	crm, err := s.HostActor(ctx, "crm")
	if err != nil {
		t.Fatal(err)
	}
	erp, _ := s.HostActor(ctx, "erp")
	post := func() {
		t.Helper()
		if _, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: user, Scope: "u1", Text: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.AppendNoteOnce(ctx, hostEvent(conv, crm, "k", "x")); !errors.Is(err, svc.ErrNoTurn) {
		t.Fatalf("no turn yet: %v", err)
	}
	post()
	first, created, err := s.AppendNoteOnce(ctx, hostEvent(conv, crm, "k", "first"))
	if err != nil || !created {
		t.Fatalf("first: created=%v %v", created, err)
	}
	other := in(hostEvent(conv, crm, "k", "second"), types.EventHostMessage)
	again, created, err := s.AppendNoteOnce(ctx, other)
	if err != nil || created || again.ID != first.ID || again.UUID != first.UUID || !again.CreatedAt.Equal(first.CreatedAt) ||
		again.Type != types.EventContextNote || string(again.Data) != string(first.Data) {
		t.Fatalf("repeat: created=%v %+v (%v), want the first %+v", created, again, err, first)
	}
	// a request is checked before its key is looked up: an invalid repeat is a 400, not the first event
	var br *svc.BadRequestError
	if _, _, err = s.AppendNoteOnce(ctx, hostEvent(conv, crm, "k", "")); !errors.As(err, &br) {
		t.Fatalf("repeat with an empty text: %v", err)
	}

	for name, ev := range map[string]svc.NoteInput{
		"another key":    hostEvent(conv, crm, "k2", "a"),
		"key case":       hostEvent(conv, crm, "K", "b"),
		"another actor":  hostEvent(conv, erp, "k", "c"),
		"same other key": hostEvent(conv, erp, "k2", "d"),
	} {
		if r, created, err := s.AppendNoteOnce(ctx, ev); err != nil || !created || r.ID == first.ID {
			t.Errorf("%s: created=%v id=%d (%v)", name, created, r.ID, err)
		}
	}

	// the turn moves on: the key still names the event it named
	if err := s.EndTurn(ctx, conv.ID, mustTurn(t, store, conv), types.TurnStateEvent{State: types.StateCompleted}); err != nil {
		t.Fatal(err)
	}
	post()
	if again, created, err = s.AppendNoteOnce(ctx, hostEvent(conv, crm, "k", "third")); err != nil || created || again.ID != first.ID {
		t.Fatalf("repeat under a newer turn: created=%v %v", created, err)
	}

	other2, _ := store.CreateConversation(ctx)
	if _, err := s.Post(ctx, svc.PostInput{ConversationID: other2.ID, Actor: user, Scope: "u1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if r, created, err := s.AppendNoteOnce(ctx, hostEvent(other2, crm, "k", "elsewhere")); err != nil || !created || r.ID == first.ID {
		t.Fatalf("another conversation: created=%v %v", created, err)
	}
}

func in(n svc.NoteInput, typ types.EventType) svc.NoteInput { n.Type = typ; return n }

func mustTurn(t *testing.T, store *db.Store, conv db.ConversationRow) int64 {
	t.Helper()
	id, err := store.LatestTurnID(t.Context(), store.Pool, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Host events never start a turn: no job is queued, however many arrive.
func TestAppendNoteOnceNeverQueuesAJob(t *testing.T) {
	ctx, store, s, conv, user := noteFixture(t)
	crm, _ := s.HostActor(ctx, "crm")
	if _, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: user, Scope: "u1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	jobs := countJobs(t, store)
	for i := range 5 {
		if _, _, err := s.AppendNoteOnce(ctx, hostEvent(conv, crm, string(rune('a'+i)), "n")); err != nil {
			t.Fatal(err)
		}
	}
	if n := countJobs(t, store); n != jobs {
		t.Fatalf("%d jobs after host events, was %d", n, jobs)
	}
}

// Concurrent sends of one key store one event: the same row comes back to all.
func TestAppendNoteOnceConcurrentSameKey(t *testing.T) {
	ctx, store, s, conv, user := noteFixture(t)
	crm, _ := s.HostActor(ctx, "crm")
	if _, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: user, Scope: "u1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	const n = 24
	var wg sync.WaitGroup
	ids, made := make([]int64, n), make([]bool, n)
	for i := range n {
		wg.Go(func() {
			r, created, err := s.AppendNoteOnce(ctx, hostEvent(conv, crm, "same", "text"))
			if err != nil {
				t.Error(err)
			}
			ids[i], made[i] = r.ID, created
		})
	}
	wg.Wait()
	creators := 0
	for i := range n {
		if made[i] {
			creators++
		}
		if ids[i] != ids[0] {
			t.Fatalf("callers got different events: %v", ids)
		}
	}
	var rows int
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_events WHERE key = 'same'`).Scan(&rows)
	if creators != 1 || rows != 1 {
		t.Fatalf("%d creators, %d rows; want 1 and 1", creators, rows)
	}
}

// A turn takes MaxHostEventsPerTurn host events and refuses the next, which
// stores nothing. A repeat still answers, other actors' events and unkeyed
// notes do not count, and the next turn starts at zero.
func TestHostEventsAreBoundedPerTurn(t *testing.T) {
	ctx, store, s, conv, user := noteFixture(t)
	crm, _ := s.HostActor(ctx, "crm")
	if _, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: user, Scope: "u1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	key := func(i int) string { return fmt.Sprintf("k%03d", i) }
	// notes that are not a host's: unkeyed, or by a person (the existing AppendNote)
	for range 3 {
		if _, err := s.AppendNote(ctx, svc.NoteInput{ConversationID: conv.ID, Actor: user, Type: types.EventContextNote, Text: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	var first db.EventRow
	for i := range svc.MaxHostEventsPerTurn {
		r, _, err := s.AppendNoteOnce(ctx, hostEvent(conv, crm, key(i), "n"))
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if i == 0 {
			first = r
		}
	}
	var before int
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_events`).Scan(&before)
	if _, _, err := s.AppendNoteOnce(ctx, hostEvent(conv, crm, "over", "n")); !errors.Is(err, svc.ErrTooManyHostEvents) {
		t.Fatalf("over the bound: %v", err)
	}
	erp, _ := s.HostActor(ctx, "erp")
	if _, _, err := s.AppendNoteOnce(ctx, hostEvent(conv, erp, "x", "n")); !errors.Is(err, svc.ErrTooManyHostEvents) {
		t.Fatalf("the bound is per turn, not per source: %v", err)
	}
	var after int
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_events`).Scan(&after)
	if after != before {
		t.Fatalf("a refused event stored %d rows", after-before)
	}
	if r, created, err := s.AppendNoteOnce(ctx, hostEvent(conv, crm, key(0), "again")); err != nil || created || r.ID != first.ID {
		t.Fatalf("a repeat at the bound: created=%v %v", created, err)
	}
	if err := s.EndTurn(ctx, conv.ID, mustTurn(t, store, conv), types.TurnStateEvent{State: types.StateCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: user, Scope: "u1", Text: "again"}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.AppendNoteOnce(ctx, hostEvent(conv, crm, "over", "n")); err != nil || !created {
		t.Fatalf("the next turn takes events again: created=%v %v", created, err)
	}
}

// A host's source name lands in the prompt, so it is a config name.
func TestHostActorRefusesBadNames(t *testing.T) {
	ctx, store, s, _, _ := noteFixture(t)
	for _, ref := range []string{"", " crm", "crm ", "a b", "cr\nm", "a\"b", "a<b", "a'b", "crm\x00", "é", "crm\u202e", "\ufeffcrm",
		"a.b", "a/b", strings.Repeat("a", 65)} {
		var br *svc.BadRequestError
		if _, err := s.HostActor(ctx, ref); !errors.As(err, &br) {
			t.Errorf("%q: want a BadRequestError, got %v", ref, err)
		}
	}
	for _, ref := range []string{"host", "HOST", "crm", "CRM", "a-b_C9", strings.Repeat("a", 64)} {
		if _, err := s.HostActor(ctx, ref); err != nil {
			t.Errorf("%q: %v", ref, err)
		}
	}
	a, _ := s.HostActor(ctx, "crm")
	b, _ := s.HostActor(ctx, "CRM")
	c, _ := s.HostActor(ctx, "crm")
	if a == b || a != c {
		t.Fatalf("refs compare exactly: crm=%d CRM=%d crm=%d", a, b, c)
	}
	var typ string
	store.Pool.QueryRow(ctx, `SELECT type FROM actors WHERE id = $1`, a).Scan(&typ)
	if typ != "host" {
		t.Fatalf("actor type %q", typ)
	}
}
