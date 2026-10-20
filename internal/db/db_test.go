package db_test

import (
	"context"
	"strings"
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
	if mt, err := msg.Payload[types.MessageText](); err != nil || mt.Text != "hi" {
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
	for i := range 2 {
		if _, err := s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: map[string]int{"n": i}, Key: "session:1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: 3, Key: "session:2"}); err != nil {
		t.Fatal(err)
	}
	j1, err := s.ClaimJob(ctx, "turns", allTypes, time.Minute)
	if err != nil || j1 == nil || j1.Attempts != 1 {
		t.Fatalf("claim 1: %+v %v", j1, err)
	}
	// session:1 has a running job, so the second claim must skip its queued job.
	j2, err := s.ClaimJob(ctx, "turns", allTypes, time.Minute)
	if err != nil || j2 == nil || *j2.ConcurrencyKey != "session:2" {
		t.Fatalf("claim 2 should pick session:2, got %+v %v", j2, err)
	}
	if j3, _ := s.ClaimJob(ctx, "turns", allTypes, time.Minute); j3 != nil {
		t.Fatalf("claim 3 should find nothing, got %+v", j3)
	}
	s.CompleteJob(ctx, j1.ID, j1.Attempts)
	if j3, _ := s.ClaimJob(ctx, "turns", allTypes, time.Minute); j3 == nil {
		t.Fatal("after completion the next session:1 job must be claimable")
	}
}

func TestReapAndRetry(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx := context.Background()
	s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobEmbedEvents, Queue: "embeddings", Payload: struct{}{}, MaxAttempts: 2})
	s.ClaimJob(ctx, "embeddings", allTypes, 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	reaped, err := s.ReapJobs(ctx)
	if err != nil || len(reaped) != 1 || reaped[0].State != types.JobQueued {
		t.Fatalf("reap: %+v %v", reaped, err)
	}
	j, _ := s.ClaimJob(ctx, "embeddings", allTypes, 50*time.Millisecond)
	if j == nil || j.Attempts != 2 {
		t.Fatalf("second claim: %+v", j)
	}
	st, _ := s.RetryJob(ctx, j.ID, j.Attempts, 0, "boom")
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
	ctx := t.Context()
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

// A worker whose lease expired must not complete, retry or extend the job once
// another worker has claimed it again: the attempt number fences it out.
func TestStaleWorkerCannotTouchAReclaimedJob(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx := context.Background()
	s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: 1, Key: "session:1", MaxAttempts: 5})
	stale, _ := s.ClaimJob(ctx, "turns", allTypes, 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if _, err := s.ReapJobs(ctx); err != nil {
		t.Fatal(err)
	}
	fresh, _ := s.ClaimJob(ctx, "turns", allTypes, time.Minute)
	if fresh == nil || fresh.Attempts != 2 {
		t.Fatalf("reclaim: %+v", fresh)
	}

	if ok, err := s.HeartbeatJob(ctx, stale.ID, stale.Attempts, time.Hour); err != nil || ok {
		t.Fatalf("stale heartbeat must report the lease lost: ok=%v err=%v", ok, err)
	}
	if err := s.CompleteJob(ctx, stale.ID, stale.Attempts); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryJob(ctx, stale.ID, stale.Attempts, 0, "late failure"); err != nil && !db.IsNotFound(err) {
		t.Fatal(err)
	}
	var state string
	s.Pool.QueryRow(ctx, "SELECT state FROM jobs WHERE id = $1", stale.ID).Scan(&state)
	if state != string(types.JobRunning) {
		t.Fatalf("the stale worker changed the reclaimed job to %q", state)
	}
	// The session's next job must still be blocked behind the running one.
	s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: 2, Key: "session:1"})
	if j, _ := s.ClaimJob(ctx, "turns", allTypes, time.Minute); j != nil {
		t.Fatalf("two jobs of one session running at once: %+v", j)
	}
	if ok, _ := s.HeartbeatJob(ctx, fresh.ID, fresh.Attempts, time.Minute); !ok {
		t.Fatal("the current owner's heartbeat must succeed")
	}
}

// Stored events carry their payload version. A row from a newer hfy (a rolling
// deploy) is served as stored, but never interpreted by this binary.
func TestEventVersions(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx)
	actor, _ := s.FindOrCreateActor(ctx, types.ActorAnonymous, types.NoScope)
	turn, err := s.AppendEvent(ctx, s.Pool, db.NewEvent{SessionID: sess.ID, ActorID: actor, Scope: "n/a", Payload: types.Turn{}})
	if err != nil || turn.V != types.Version(types.EventTurn) {
		t.Fatalf("append: v%d %v", turn.V, err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO session_events (session_id, parent_id, actor_id, scope, type, data, v)
		VALUES ($1, $2, $3, 'n/a', 'message.text', '{"text":"hi","lang":"en"}', 9)`, sess.ID, turn.ID, actor); err != nil {
		t.Fatal(err)
	}
	evs, err := s.TurnEvents(ctx, s.Pool, turn.ID)
	if err != nil || len(evs) != 2 {
		t.Fatalf("%d events, %v", len(evs), err)
	}
	newer := evs[1]
	if newer.V != 9 || newer.Envelope().V != 9 {
		t.Fatalf("a newer payload must keep its version: %d", newer.V)
	}
	if _, err := newer.Payload[types.MessageText](); err == nil || !strings.Contains(err.Error(), "v9") {
		t.Fatalf("decoding a newer payload must fail loudly, got %v", err)
	}
	if mt, err := evs[0].Payload[types.Turn](); err != nil {
		t.Fatalf("current payload: %v %v", mt, err)
	}
}

var allTypes = []types.JobType{types.JobRunTurn, types.JobApprovalTimeout, types.JobEmbedEvents, types.JobRetentionClean}

// A worker only claims job types it can run: an older worker in a rolling
// deploy leaves a newer job type alone instead of failing it.
func TestClaimOnlyHandledTypes(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx := context.Background()
	s.Enqueue(ctx, s.Pool, db.NewJob{Type: "run_turn.v2", Queue: "turns", Payload: 1})
	if j, err := s.ClaimJob(ctx, "turns", allTypes, time.Minute); err != nil || j != nil {
		t.Fatalf("claimed an unknown job type: %+v %v", j, err)
	}
	if j, _ := s.ClaimJob(ctx, "turns", []types.JobType{"run_turn.v2"}, time.Minute); j == nil {
		t.Fatal("a worker that handles the type must claim it")
	}
}
