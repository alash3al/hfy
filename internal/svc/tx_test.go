package svc_test

import (
	"context"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// Every session mutation must run on its transaction's connection alone. A
// read that takes a second connection from the pool while the transaction
// holds one deadlocks once the pool is exhausted; with a one-connection pool
// it deadlocks every time.
func TestMutationsNeedOneConnection(t *testing.T) {
	big := dbtest.New(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := db.Open(ctx, big.Pool.Config().ConnConfig.ConnString(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{Queue: config.Queue{MaxAttempts: 3}}
	s := svc.New(store, cfg)
	// The agent's actor is left uncached on purpose: a mutation must resolve
	// it on its transaction's connection.
	user, _ := store.FindOrCreateActor(ctx, store.Pool, types.ActorAnonymous, types.NoScope)

	step := func(name string, fn func(context.Context) error) {
		t.Helper()
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := fn(c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	sess, _ := store.CreateSession(ctx)
	var turn int64
	step("post", func(c context.Context) error {
		u, err := s.Post(c, svc.PostInput{SessionID: sess.ID, Actor: user, Scope: types.NoScope, Text: "hi"})
		if err == nil {
			turn, err = store.TurnIDByUUID(c, sess.ID, u)
		}
		return err
	})
	step("start", func(c context.Context) error { _, err := s.StartTurn(c, sess.ID, turn); return err })
	step("end", func(c context.Context) error {
		return s.EndTurn(c, sess.ID, turn, types.TurnStateEvent{State: types.StateCompleted})
	})
	step("post again", func(c context.Context) error {
		_, err := s.Post(c, svc.PostInput{SessionID: sess.ID, Actor: user, Scope: types.NoScope, Text: "again"})
		return err
	})
	step("stop", func(c context.Context) error { return s.Stop(c, sess.ID, user) })
}
