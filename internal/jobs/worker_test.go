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
