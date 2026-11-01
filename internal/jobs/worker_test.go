package jobs_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/jobs"
	"github.com/alash3al/hfy/internal/types"
)

func TestWorkerRunsRetriesAndExhausts(t *testing.T) {
	s := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w := jobs.NewWorker(s, time.Second, []string{"turns"})
	var okRuns, badRuns atomic.Int32
	exhausted := make(chan struct{})
	type p struct{ N int }
	w.Handle(types.JobRunTurn, func(ctx context.Context, p p, j db.JobRow) error {
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
	s := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w := jobs.NewWorker(s, 300*time.Millisecond, []string{"turns"})
	started, stopped := make(chan struct{}), make(chan error, 1)
	w.Handle(types.JobRunTurn, func(ctx context.Context, _ struct{}, j db.JobRow) error {
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
	other, _ := s.ClaimJob(ctx, "turns", allTypes, time.Minute)
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
	s := dbtest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	w := jobs.NewWorker(s, time.Minute, []string{"turns"})
	started := make(chan struct{})
	w.Handle(types.JobRunTurn, func(ctx context.Context, _ struct{}, _ db.JobRow) error {
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
	j, err := s.ClaimJob(context.Background(), "turns", allTypes, time.Minute)
	if err != nil || j == nil {
		t.Fatalf("the job must be claimable at once after shutdown: %+v %v", j, err)
	}
}

var allTypes = []types.JobType{types.JobRunTurn, types.JobApprovalTimeout, types.JobRetentionClean}

// However hostile the error text, the failure is recorded: the job leaves
// 'running' with its attempt counted and a last_error that is valid, NUL-free,
// bounded, and free of URLs.
func TestWorkerRecordsFailuresWithCleanBoundedText(t *testing.T) {
	s := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	errs := map[string]error{
		"nul":       errors.New("bad\x00byte"),
		"surrogate": errors.New("lone \xed\xa0\x80 surrogate and \xff"),
		"huge":      errors.New(strings.Repeat("x", 100<<10)),
		"runes":     errors.New(strings.Repeat("€", 100<<10)),
		"url": &url.Error{Op: "Post", URL: "https://hook.example/p?token=SECRET&x=\"q\"",
			Err: errors.New("connection refused")},
	}
	w := jobs.NewWorker(s, time.Minute, []string{"turns"})
	type p struct{ Name string }
	w.Handle(types.JobRunTurn, func(ctx context.Context, p p, j db.JobRow) error { return errs[p.Name] })
	for name := range errs {
		s.Enqueue(ctx, s.Pool, db.NewJob{Type: types.JobRunTurn, Queue: "turns", Payload: p{name}, MaxAttempts: 1})
	}
	go w.Run(ctx, 2)

	for {
		var n int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE state = 'failed'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == len(errs) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("only %d of %d failures recorded", n, len(errs))
		case <-time.After(50 * time.Millisecond):
		}
	}
	rows, err := s.Pool.Query(ctx, `SELECT payload->>'Name', attempts, last_error FROM jobs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, last string
		var attempts int
		if err := rows.Scan(&name, &attempts, &last); err != nil {
			t.Fatal(err)
		}
		if attempts != 1 || len(last) == 0 || len(last) > 2<<10 || strings.ContainsRune(last, 0) ||
			!utf8.ValidString(last) || strings.Contains(last, "SECRET") || strings.Contains(last, "hook.example") {
			t.Errorf("%s: attempts=%d last_error (%d bytes) %.80q", name, attempts, len(last), last)
		}
		if name == "url" && last != `Post "<url>": connection refused` {
			t.Errorf("url error recorded as %q", last)
		}
	}
}
