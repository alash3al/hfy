// Package jobs is the Postgres-backed queue worker: claim with SKIP LOCKED,
// lease with heartbeat, a lease reaper, and wake-ups through LISTEN/NOTIFY.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/types"
)

type Handler func(ctx context.Context, job db.JobRow) error

// ExhaustedFunc runs once a job has failed for good (out of attempts).
type ExhaustedFunc func(ctx context.Context, job db.JobRow, err error)

type Worker struct {
	store  *db.Store
	lease  time.Duration
	queues []string

	handlers  map[types.JobType]Handler
	exhausted map[types.JobType]ExhaustedFunc
	wake      chan struct{}
}

func NewWorker(store *db.Store, lease time.Duration, queues []string) *Worker {
	return &Worker{
		store: store, lease: lease, queues: queues,
		handlers:  map[types.JobType]Handler{},
		exhausted: map[types.JobType]ExhaustedFunc{},
		wake:      make(chan struct{}, 64),
	}
}

// Handle registers a handler that receives the job payload decoded into P.
func Handle[P any](w *Worker, t types.JobType, fn func(ctx context.Context, p P, job db.JobRow) error) {
	w.handlers[t] = func(ctx context.Context, job db.JobRow) error {
		var p P
		if len(job.Payload) > 0 {
			if err := json.Unmarshal(job.Payload, &p); err != nil {
				return fmt.Errorf("decode %s payload: %w", t, err)
			}
		}
		return fn(ctx, p, job)
	}
}

// OnExhausted registers a hook for jobs of type t that failed permanently.
func (w *Worker) OnExhausted(t types.JobType, fn ExhaustedFunc) { w.exhausted[t] = fn }

// Wake nudges idle workers in this process.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run starts n workers, the lease reaper and the NOTIFY listener, and blocks
// until ctx is cancelled and all workers have stopped.
func (w *Worker) Run(ctx context.Context, n int) {
	var wg sync.WaitGroup
	wg.Go(func() {
		w.store.Listen(ctx, func(_, _ string) { w.Wake() }, w.Wake, db.ChanJobs)
	})
	wg.Go(func() { w.reaper(ctx) })
	for i := 0; i < n; i++ {
		wg.Go(func() { w.loop(ctx) })
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context) {
	idle := time.NewTicker(2 * time.Second) // safety net and delayed run_at jobs
	defer idle.Stop()
	for ctx.Err() == nil {
		job := w.claim(ctx)
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
			case <-idle.C:
			}
			continue
		}
		w.run(ctx, *job)
	}
}

// claim tries the queues in priority order and returns the first job found.
func (w *Worker) claim(ctx context.Context) *db.JobRow {
	for _, q := range w.queues {
		job, err := w.store.ClaimJob(ctx, q, w.lease)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("claim job", "queue", q, "err", err)
			}
			continue
		}
		if job != nil {
			return job
		}
	}
	return nil
}

func (w *Worker) run(parent context.Context, job db.JobRow) {
	h, ok := w.handlers[job.Type]
	if !ok {
		w.fail(parent, job, fmt.Errorf("no handler for job type %q", job.Type))
		return
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	go w.heartbeat(ctx, job.ID, done)

	err := safely(func() error { return h(ctx, job) })
	close(done)

	// Shutdown mid-job: leave it running; the lease expires and the reaper requeues it.
	if err != nil && parent.Err() != nil {
		return
	}
	wctx := context.WithoutCancel(parent)
	if err != nil {
		w.fail(wctx, job, err)
		return
	}
	if err := w.store.CompleteJob(wctx, job.ID); err != nil {
		slog.Error("complete job", "job", job.ID, "err", err)
	}
	w.Wake() // the same session may have a queued job behind this one
}

func (w *Worker) fail(ctx context.Context, job db.JobRow, err error) {
	ctx = context.WithoutCancel(ctx)
	slog.Error("job failed", "job", job.ID, "type", job.Type, "attempt", job.Attempts, "err", err)
	delay := min(time.Duration(job.Attempts*job.Attempts)*2*time.Second, time.Minute)
	state, rerr := w.store.RetryJob(ctx, job.ID, delay, err.Error())
	if rerr != nil {
		slog.Error("retry job", "job", job.ID, "err", rerr)
		return
	}
	if state == types.JobFailed {
		w.runExhausted(ctx, job, err)
	}
}

func (w *Worker) runExhausted(ctx context.Context, job db.JobRow, err error) {
	if fn := w.exhausted[job.Type]; fn != nil {
		_ = safely(func() error { fn(ctx, job, err); return nil })
	}
}

func (w *Worker) heartbeat(ctx context.Context, id int64, done <-chan struct{}) {
	t := time.NewTicker(w.lease / 3)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.store.HeartbeatJob(context.WithoutCancel(ctx), id, w.lease); err != nil {
				slog.Warn("heartbeat", "job", id, "err", err)
			}
		}
	}
}

// reaper requeues jobs whose worker died. It is a timer, not a job: if every
// worker had died there would be nobody to run a job.
func (w *Worker) reaper(ctx context.Context) {
	t := time.NewTicker(w.lease)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		jobs, err := w.store.ReapJobs(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("reap jobs", "err", err)
			}
			continue
		}
		for _, j := range jobs {
			slog.Warn("lease expired", "job", j.ID, "type", j.Type, "state", j.State)
			if j.State == types.JobFailed {
				w.runExhausted(ctx, j, fmt.Errorf("lease expired after %d attempts", j.Attempts))
			}
		}
		if len(jobs) > 0 {
			w.Wake()
		}
	}
}

func safely(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return fn()
}
