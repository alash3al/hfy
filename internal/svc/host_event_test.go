package svc_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

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

// A turn takes Limit notes and messages from hosts and refuses the next, which
// stores nothing. Notes of other actors do not count, and the next turn starts
// at zero. No host event queues a job.
func TestHostEventsAreBoundedPerTurn(t *testing.T) {
	ctx, store, s, conv, user := noteFixture(t)
	crm, _ := s.HostActor(ctx, "crm")
	erp, _ := s.HostActor(ctx, "erp")
	if _, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: user, Scope: "u1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	jobs := countJobs(t, store)
	note := func(actor int64, limit int) error {
		_, err := s.AppendNote(ctx, svc.NoteInput{ConversationID: conv.ID, Actor: actor, Type: types.EventContextNote, Text: "n", Limit: limit})
		return err
	}
	for range 3 { // a person's notes are not a host's
		if err := note(user, 0); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 4 {
		if err := note(crm, 4); err != nil {
			t.Fatalf("note %d: %v", i, err)
		}
	}
	var before int
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_events`).Scan(&before)
	for _, who := range []int64{crm, erp} { // per turn, not per source
		if err := note(who, 4); !errors.Is(err, svc.ErrTooManyHostEvents) {
			t.Fatalf("over the bound: %v", err)
		}
	}
	var after int
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_events`).Scan(&after)
	if after != before || countJobs(t, store) != jobs {
		t.Fatalf("refused notes stored %d events; jobs %d, was %d", after-before, countJobs(t, store), jobs)
	}
	if err := s.EndTurn(ctx, conv.ID, mustTurn(t, store, conv), types.TurnStateEvent{State: types.StateCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: user, Scope: "u1", Text: "again"}); err != nil {
		t.Fatal(err)
	}
	if err := note(crm, 4); err != nil {
		t.Fatalf("the next turn takes notes again: %v", err)
	}
}

func mustTurn(t *testing.T, store *db.Store, conv db.ConversationRow) int64 {
	t.Helper()
	id, err := store.LatestTurnID(t.Context(), store.Pool, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
