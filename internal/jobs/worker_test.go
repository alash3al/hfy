package jobs_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/jobs"
	"github.com/alash3al/hfy/internal/types"
)

func TestWorkerRunsRetriesAndExhausts(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w := jobs.NewWorker(s, time.Second, []string{"turns"})
	var okRuns, badRuns atomic.Int32
	exhausted := make(chan struct{})
	type p struct{ N int }
	jobs.Handle(w, types.JobRunTurn, func(ctx context.Context, p p, j db.JobRow) error {
		if p.N == 1 {
			okRuns.Add(1)
			return nil
		}
		badRuns.Add(1)
		return errors.New("always fails")
	})
	w.OnExhausted(types.JobRunTurn, func(context.Context, db.JobRow, error) { close(exhausted) })

	s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: p{1}, MaxAttempts: 2})
	s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: p{2}, MaxAttempts: 2})

	go w.Run(ctx, 2)
	select {
	case <-exhausted:
	case <-ctx.Done():
		t.Fatal("job never exhausted")
	}
	if okRuns.Load() != 1 || badRuns.Load() != 2 {
		t.Fatalf("ok=%d bad=%d", okRuns.Load(), badRuns.Load())
	}
}

// A handler whose job was reaped and claimed again is cancelled at its next
// heartbeat, and its late result does not touch the job.
func TestWorkerStopsAHandlerThatLostItsLease(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w := jobs.NewWorker(s, 300*time.Millisecond, []string{"turns"})
	started, stopped := make(chan struct{}), make(chan error, 1)
	jobs.Handle(w, types.JobRunTurn, func(ctx context.Context, _ struct{}, j db.JobRow) error {
		if j.Attempts > 1 {
			<-ctx.Done() // the new owner keeps running
			return ctx.Err()
		}
		close(started)
		<-ctx.Done()
		stopped <- context.Cause(ctx)
		return ctx.Err()
	})
	s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: struct{}{}, MaxAttempts: 5})
	go w.Run(ctx, 1)
	<-started

	// Another instance's reaper requeues it and another worker claims it.
	if _, err := s.Pool.Exec(ctx, "UPDATE jobs SET state = 'queued', lease_until = NULL"); err != nil {
		t.Fatal(err)
	}
	other, _ := s.ClaimJob(ctx, "turns", time.Minute)
	if other == nil {
		t.Fatal("reclaim failed")
	}
	select {
	case cause := <-stopped:
		if cause == nil || cause.Error() != "job lease lost" {
			t.Fatalf("cause: %v", cause)
		}
	case <-ctx.Done():
		t.Fatal("the stale handler was never cancelled")
	}
	time.Sleep(200 * time.Millisecond)
	var state string
	var attempts int
	s.Pool.QueryRow(ctx, "SELECT state, attempts FROM jobs").Scan(&state, &attempts)
	if state != "running" || attempts != 2 {
		t.Fatalf("job is %s/%d, want running/2 (owned by the new claim)", state, attempts)
	}
}

// Shutdown hands an interrupted job straight back to the queue.
func TestWorkerReleasesJobsOnShutdown(t *testing.T) {
	s := dbtest.New(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	w := jobs.NewWorker(s, time.Minute, []string{"turns"})
	started := make(chan struct{})
	jobs.Handle(w, types.JobRunTurn, func(ctx context.Context, _ struct{}, _ db.JobRow) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	s.Enqueue(context.Background(), s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: struct{}{}})
	done := make(chan struct{})
	go func() { w.Run(ctx, 1); close(done) }()
	<-started
	cancel()
	<-done
	j, err := s.ClaimJob(context.Background(), "turns", time.Minute)
	if err != nil || j == nil {
		t.Fatalf("the job must be claimable at once after shutdown: %+v %v", j, err)
	}
}
