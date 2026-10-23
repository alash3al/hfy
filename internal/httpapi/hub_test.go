package httpapi_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/httpapi"
	"github.com/alash3al/hfy/internal/types"
)

// Notify only queues, so the listener never waits on the database; Run
// delivers what was queued, in the order it was notified.
func TestHubDeliversQueuedEventsInOrder(t *testing.T) {
	ctx := t.Context()
	store := dbtest.New(t, nil)
	conv, err := store.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := store.FindOrCreateActor(ctx, store.Pool, types.ActorExternal, "u1")
	hub := httpapi.NewHub(store)
	sub, unsubscribe := hub.Subscribe(conv.ID)
	defer unsubscribe()

	var want []int64
	for range 3 {
		row, err := store.AppendEvent(ctx, store.Pool, db.NewEvent{ConversationID: conv.ID, ActorID: actor, Scope: "u1", Payload: types.MessageText{Text: "x"}})
		if err != nil {
			t.Fatal(err)
		}
		hub.Notify(ctx, fmt.Sprintf("%d:%d", conv.ID, row.ID))
		want = append(want, row.ID)
	}
	if len(sub.C) > 0 {
		t.Fatal("Notify delivered by itself: loading the event is Run's job")
	}
	go hub.Run(ctx)
	for _, id := range want {
		select {
		case row := <-sub.C:
			if row.ID != id {
				t.Fatalf("got event %d, want %d", row.ID, id)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not deliver a queued event")
		}
	}
}
