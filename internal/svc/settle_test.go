package svc_test

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// settled_until only lets the turn queries skip turns that have ended. Under
// a random mix of posts by several people, starts, completions, turns failing
// out of order and stops, both queries must answer exactly as a full scan of
// the conversation does, after every step.
func TestSettledTurnsMatchAFullScan(t *testing.T) {
	store := dbtest.New(t, nil)
	ctx := t.Context()
	s := svc.New(store, &config.Config{Queue: config.Queue{MaxAttempts: 3}})
	conv, _ := store.CreateConversation(ctx)
	var people []int64
	for _, ref := range []string{"u1", "u2", "u3"} {
		id, _ := store.FindOrCreateActor(ctx, store.Pool, types.ActorExternal, ref)
		people = append(people, id)
	}

	watermark := func() (w int64) {
		store.Pool.QueryRow(ctx, "SELECT settled_until FROM conversations WHERE id = $1", conv.ID).Scan(&w)
		return w
	}
	answers := func() (open, queued []int64) {
		open, err1 := store.OpenTurns(ctx, store.Pool, conv.ID, 1000)
		queued, err2 := store.QueuedTurns(ctx, store.Pool, conv.ID, 0)
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		return open, queued
	}
	check := func(step int, op string) {
		t.Helper()
		w := watermark()
		open, queued := answers()
		store.Pool.Exec(ctx, "UPDATE conversations SET settled_until = 0 WHERE id = $1", conv.ID)
		fullOpen, fullQueued := answers()
		store.Pool.Exec(ctx, "UPDATE conversations SET settled_until = $2 WHERE id = $1", conv.ID, w)
		if !slices.Equal(open, fullOpen) || !slices.Equal(queued, fullQueued) {
			t.Fatalf("step %d (%s), settled_until=%d: open %v queued %v, a full scan says open %v queued %v",
				step, op, w, open, queued, fullOpen, fullQueued)
		}
	}

	rnd := rand.New(rand.NewPCG(7, 11))
	for step := range 400 {
		open, _ := answers()
		cur, _ := store.ConversationByID(ctx, conv.ID)
		var op string
		var err error
		switch n := rnd.IntN(10); {
		case n < 4:
			op = "post"
			_, err = s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: people[rnd.IntN(3)], Scope: "u1", Text: "hi"})
		case n < 6 && len(open) > 0:
			op = "start"
			_, err = s.StartTurn(ctx, conv.ID, open[0])
		case n < 8 && cur.ActiveTurnID != nil:
			op = "complete"
			err = s.EndTurn(ctx, conv.ID, *cur.ActiveTurnID, types.TurnStateEvent{State: types.StateCompleted})
		case n < 9 && len(open) > 1:
			op = "fail out of order" // a later turn ends while an earlier one is still open
			err = s.EndTurn(ctx, conv.ID, open[1+rnd.IntN(len(open)-1)], types.TurnStateEvent{State: types.StateFailed})
		default:
			op = "stop"
			err = s.Stop(ctx, conv.ID, people[rnd.IntN(3)])
		}
		if err != nil {
			t.Fatalf("step %d (%s): %v", step, op, err)
		}
		check(step, op)
	}
	if watermark() == 0 {
		t.Fatal("the watermark never moved: the queries still scan every turn")
	}
}
