package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// Two turns queued, none running: the note is in the prompt of the turn that
// runs first.
func TestNoteReachesTheFirstQueuedTurn(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New(t)
	cfg := &config.Config{Queue: config.Queue{MaxAttempts: 1}}
	rt := &Runtime{Cfg: cfg, Store: store, Svc: svc.New(store, cfg)}
	conv, err := store.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := store.FindOrCreateActor(ctx, store.Pool, types.ActorExternal, "u1")
	var turns []int64
	for _, text := range []string{"A", "B"} {
		u, err := rt.Svc.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: user, Scope: "u1", Text: text})
		if err != nil {
			t.Fatal(err)
		}
		id, _ := store.TurnIDByUUID(ctx, conv.ID, u)
		turns = append(turns, id)
	}
	if _, err := rt.Svc.AppendNote(ctx, svc.NoteInput{ConversationID: conv.ID, Actor: user, Type: types.EventContextNote, Text: "FOR-A"}); err != nil {
		t.Fatal(err)
	}
	_, msgs, err := rt.buildContext(ctx, conv.ID, turns[0], &Spec{Name: "main", System: "s"})
	if err != nil || !strings.Contains(fmt.Sprint(msgs), "FOR-A</context>") {
		t.Fatalf("the first turn's prompt lacks the note: %v %v", msgs, err)
	}
}
