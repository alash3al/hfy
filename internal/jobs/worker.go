// Package jobs is the Postgres-backed queue worker: claim with SKIP LOCKED,
// lease with heartbeat, a lease reaper, and wake-ups through LISTEN/NOTIFY.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/metrics"
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

// Handle registers a handler that receives the job payload decoded into P
// (inferred from fn).
func (w *Worker) Handle[P any](t types.JobType, fn func(ctx context.Context, p P, job db.JobRow) error) {
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

// Run starts n workers and the lease reaper, and blocks until ctx is
// cancelled and all workers have stopped. The caller's listener calls Wake on
// db.ChanJobs notifications.
func (w *Worker) Run(ctx context.Context, n int) {
	var wg sync.WaitGroup
	wg.Go(func() { w.reaper(ctx) })
	for range n {
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
	handled := make([]types.JobType, 0, len(w.handlers))
	for t := range w.handlers {
		handled = append(handled, t)
	}
	for _, q := range w.queues {
		job, err := w.store.ClaimJob(ctx, q, handled, w.lease)
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

// errLeaseLost cancels a handler whose job was reaped and may run elsewhere.
var errLeaseLost = errors.New("job lease lost")

func (w *Worker) run(parent context.Context, job db.JobRow) {
	h := w.handlers[job.Type] // claimed only because it is handled
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	done := make(chan struct{})
	go w.heartbeat(ctx, job, cancel, done)

	start := time.Now()
	err := safely(func() error { return h(ctx, job) })
	close(done)
	metrics.Since(metrics.JobDuration.WithLabelValues(string(job.Type)), start)

	wctx := context.WithoutCancel(parent)
	switch {
	case err != nil && parent.Err() != nil:
		// Shutdown mid-job: hand it back now rather than when its lease expires.
		if rerr := w.store.ReleaseJob(wctx, job.ID, job.Attempts); rerr != nil {
			slog.Error("release job", "job", job.ID, "err", rerr)
		}
		count(job, "released")
		return
	case errors.Is(context.Cause(ctx), errLeaseLost):
		slog.Warn("job lease lost; its new owner finishes it", "job", job.ID, "type", job.Type)
		count(job, "lost")
		return
	case err != nil:
		w.fail(wctx, job, err)
		return
	}
	if err := w.store.CompleteJob(wctx, job.ID, job.Attempts); err != nil {
		slog.Error("complete job", "job", job.ID, "err", err)
	}
	count(job, "done")
	w.Wake() // the same conversation may have a queued job behind this one
}

func (w *Worker) fail(ctx context.Context, job db.JobRow, err error) {
	ctx = context.WithoutCancel(ctx)
	slog.Error("job failed", "job", job.ID, "type", job.Type, "attempt", job.Attempts,
		"payload", string(job.Payload), "err", err)
	delay := min(time.Duration(job.Attempts*job.Attempts)*2*time.Second, time.Minute)
	state, rerr := w.store.RetryJob(ctx, job.ID, job.Attempts, delay, err.Error())
	if db.IsNotFound(rerr) {
		count(job, "lost") // reaped meanwhile: no longer ours to retry
		return
	}
	if rerr != nil {
		slog.Error("retry job", "job", job.ID, "err", rerr)
		return
	}
	if state == types.JobFailed {
		count(job, "failed")
		w.runExhausted(ctx, job, err)
		return
	}
	count(job, "retry")
}

func count(job db.JobRow, outcome string) {
	metrics.Jobs.WithLabelValues(string(job.Type), outcome).Inc()
}

func (w *Worker) runExhausted(ctx context.Context, job db.JobRow, err error) {
	if fn := w.exhausted[job.Type]; fn != nil {
		_ = safely(func() error { fn(ctx, job, err); return nil })
	}
}

// heartbeat extends the lease every lease/3 and cancels the handler if the
// job is no longer ours.
func (w *Worker) heartbeat(ctx context.Context, job db.JobRow, cancel context.CancelCauseFunc, done <-chan struct{}) {
	t := time.NewTicker(w.lease / 3)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			ok, err := w.store.HeartbeatJob(context.WithoutCancel(ctx), job.ID, job.Attempts, w.lease)
			switch {
			case err != nil:
				slog.Warn("heartbeat", "job", job.ID, "err", err)
			case !ok:
				cancel(errLeaseLost)
				return
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
			count(j, "expired")
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
