package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/types"
)

func TestEventsAndActors(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx := context.Background()
	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := s.FindOrCreateActor(ctx, types.ActorExternal, "u1")
	a2, _ := s.FindOrCreateActor(ctx, types.ActorExternal, "u1")
	if a1 != a2 {
		t.Fatal("actor find-or-create not stable")
	}
	turn, err := s.AppendEvent(ctx, s.Pool, db.NewEvent{SessionID: sess.ID, ActorID: a1, Scope: "n/a", Payload: types.Turn{}})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := s.AppendEvent(ctx, s.Pool, db.NewEvent{SessionID: sess.ID, ParentID: &turn.ID, ActorID: a1, Scope: "n/a",
		Payload: types.MessageText{Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, s.Pool, db.NewEvent{SessionID: sess.ID, ParentID: &turn.ID, ActorID: a1, Scope: "n/a",
		Payload: types.TextDelta{Text: "h"}}); err != nil {
		t.Fatal(err)
	}
	env := msg.Envelope()
	if env.Parent == nil || *env.Parent != turn.UUID || env.Actor.Ref != "u1" {
		t.Fatalf("bad envelope %+v", env)
	}
	if mt, err := types.PayloadOf[types.MessageText](env); err != nil || mt.Text != "hi" {
		t.Fatalf("payload: %v %v", mt, err)
	}
	hist, _ := s.HistoryEvents(ctx, sess.ID, []int64{turn.ID})
	if len(hist) != 2 { // ephemeral delta excluded
		t.Fatalf("history has %d events", len(hist))
	}
	after, _ := s.EventsAfter(ctx, sess.ID, 0, 100)
	if len(after) != 3 {
		t.Fatalf("replay has %d events", len(after))
	}
	id, err := s.EventIDByUUID(ctx, sess.ID, msg.UUID)
	if err != nil || id != msg.ID {
		t.Fatal("uuid lookup", err)
	}
}

func TestJobsClaimAndConcurrency(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: map[string]int{"n": i}, Key: "session:1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: 3, Key: "session:2"}); err != nil {
		t.Fatal(err)
	}
	j1, err := s.ClaimJob(ctx, "turns", time.Minute)
	if err != nil || j1 == nil || j1.Attempts != 1 {
		t.Fatalf("claim 1: %+v %v", j1, err)
	}
	// session:1 has a running job, so the second claim must skip its queued job.
	j2, err := s.ClaimJob(ctx, "turns", time.Minute)
	if err != nil || j2 == nil || *j2.ConcurrencyKey != "session:2" {
		t.Fatalf("claim 2 should pick session:2, got %+v %v", j2, err)
	}
	if j3, _ := s.ClaimJob(ctx, "turns", time.Minute); j3 != nil {
		t.Fatalf("claim 3 should find nothing, got %+v", j3)
	}
	s.CompleteJob(ctx, j1.ID)
	if j3, _ := s.ClaimJob(ctx, "turns", time.Minute); j3 == nil {
		t.Fatal("after completion the next session:1 job must be claimable")
	}
}

func TestReapAndRetry(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx := context.Background()
	s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobEmbedEvents, Queue: "embeddings", Payload: struct{}{}, MaxAttempts: 2})
	j, _ := s.ClaimJob(ctx, "embeddings", 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	reaped, err := s.ReapJobs(ctx)
	if err != nil || len(reaped) != 1 || reaped[0].State != types.JobQueued {
		t.Fatalf("reap: %+v %v", reaped, err)
	}
	j, _ = s.ClaimJob(ctx, "embeddings", 50*time.Millisecond)
	if j == nil || j.Attempts != 2 {
		t.Fatalf("second claim: %+v", j)
	}
	st, _ := s.RetryJob(ctx, j.ID, 0, "boom")
	if st != types.JobFailed {
		t.Fatalf("exhausted job must fail, got %s", st)
	}
}

func TestDedupe(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx := context.Background()
	nj := db.NewJob{Type: types.JobRetentionClean, Queue: "maintenance", Payload: struct{}{}, DedupeStates: []types.JobState{types.JobQueued, types.JobRunning}}
	ok1, _ := s.Enqueue(ctx, s.Pool, nj)
	ok2, _ := s.Enqueue(ctx, s.Pool, nj)
	if !ok1 || ok2 {
		t.Fatalf("dedupe: %v %v", ok1, ok2)
	}
}

func TestNotify(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 4)
	go s.Listen(ctx, func(ch, p string) { got <- ch + ":" + p }, nil, db.ChanEvents)
	time.Sleep(300 * time.Millisecond)
	sess, _ := s.CreateSession(ctx)
	a, _ := s.FindOrCreateActor(ctx, types.ActorAnonymous, "n/a")
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		_, err := s.AppendEvent(ctx, tx, db.NewEvent{SessionID: sess.ID, ActorID: a, Scope: "n/a", Payload: types.Turn{}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m == "" {
			t.Fatal("empty")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no notification")
	}
}

func TestEmbeddingsSearch(t *testing.T) {
	s := dbtest.New(t, &db.Embeddings{Model: "m", Dims: 3})
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx)
	a, _ := s.FindOrCreateActor(ctx, types.ActorAnonymous, "n/a")
	var ids []int64
	for _, txt := range []string{"apples", "boats"} {
		e, err := s.AppendEvent(ctx, s.Pool, db.NewEvent{SessionID: sess.ID, ActorID: a, Scope: "n/a", Payload: types.MessageText{Text: txt}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	missing, err := s.MissingEmbeddings(ctx, 0, 10)
	if err != nil || len(missing) != 2 {
		t.Fatalf("missing: %v %v", missing, err)
	}
	s.UpsertEmbedding(ctx, ids[0], "n/a", []float32{1, 0, 0})
	s.UpsertEmbedding(ctx, ids[1], "n/a", []float32{0, 1, 0})
	hits, err := s.SearchEmbeddings(ctx, "n/a", []float32{0.9, 0.1, 0}, 1)
	if err != nil || len(hits) != 1 || hits[0].Text != "apples" {
		t.Fatalf("search: %+v %v", hits, err)
	}
	if hits, _ := s.SearchEmbeddings(ctx, "other", []float32{1, 0, 0}, 5); len(hits) != 0 {
		t.Fatal("scope filter leaked")
	}
}

func TestQueriesPlanUseIndexes(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx := context.Background()
	for _, q := range []string{
		`SELECT id FROM session_events WHERE session_id = 1 AND id > 0 AND NOT ephemeral ORDER BY id`,
		`SELECT id FROM session_events WHERE uuid = gen_random_uuid()`,
		`SELECT id FROM jobs WHERE state = 'queued' AND queue = 'turns' AND run_at <= now() ORDER BY run_at, id LIMIT 1`,
	} {
		rows, err := s.Pool.Query(ctx, "EXPLAIN "+q)
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
}
