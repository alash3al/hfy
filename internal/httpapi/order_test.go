package httpapi_test

import (
	"context"
	"testing"
	"time"

	"uuid"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/types"
)

// An event whose transaction commits after a later-numbered event must still
// reach a live stream (bigserial ids are assigned at insert, not at commit).
func TestStreamDeliversEventsCommittedOutOfIDOrder(t *testing.T) {
	a := start(t, nil)
	sid := a.newSession()
	ctx := context.Background()
	store := a.rt.Store
	u, _ := uuid.Parse(sid)
	sess, _ := store.SessionByUUID(ctx, u)
	actor, _ := store.FindOrCreateActor(ctx, store.Pool, types.ActorAnonymous, types.NoScope)

	events, closeStream := a.stream("/v1/sessions/"+sid+"/events", nil)
	defer closeStream()

	// tx1 takes the lower id but commits last
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.AppendEvent(ctx, tx, db.NewEvent{SessionID: sess.ID, ActorID: actor, Scope: "n/a", Payload: types.Turn{}})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	// a concurrent append from the worker side
	go store.AppendEvent(ctx, store.Pool, db.NewEvent{SessionID: sess.ID, ActorID: actor, Scope: "n/a", Payload: types.MessageText{Text: "later"}})
	time.Sleep(500 * time.Millisecond)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	deadline := time.After(8 * time.Second) // covers the 5 s catch-up
	for len(got) < 2 {
		select {
		case ev := <-events:
			got[ev.Type] = true
		case <-deadline:
			t.Fatalf("stream got %v; an event was lost", got)
		}
	}
}
