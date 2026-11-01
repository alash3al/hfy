package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"gocloud.dev/blob"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/jobs"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// RegisterJobs wires every job type to its handler.
func (rt *Runtime) RegisterJobs(w *jobs.Worker) {
	w.Handle(types.JobRunTurn, rt.RunTurn)
	w.OnExhausted(types.JobRunTurn, rt.onTurnExhausted)

	w.Handle(types.JobApprovalTimeout, func(ctx context.Context, p svc.ApprovalTimeoutPayload, _ db.JobRow) error {
		return rt.Svc.ApprovalTimeout(ctx, p)
	})
	w.Handle(types.JobRetentionClean, func(ctx context.Context, _ struct{}, _ db.JobRow) error {
		return rt.retention(ctx)
	})
	w.OnExhausted(types.JobRetentionClean, func(ctx context.Context, _ db.JobRow, _ error) {
		rt.scheduleRetention(ctx) // a permanent failure must not end the cycle
	})
}

func (rt *Runtime) scheduleRetention(ctx context.Context) {
	at := time.Now().Add(time.Hour)
	_, err := rt.Store.Enqueue(ctx, rt.Store.Pool, db.NewJob{
		Type: types.JobRetentionClean, Queue: types.QueueMaintenance, Payload: struct{}{},
		RunAt: &at, MaxAttempts: rt.Cfg.Queue.MaxAttempts, Key: "retention_cleanup",
		DedupeStates: []types.JobState{types.JobQueued},
	})
	if err != nil {
		slog.Error("schedule retention", "err", err)
	}
}

// retention deletes expired ephemeral events, trace rows and finished jobs,
// plus expired upload slots in the bucket, then re-enqueues itself. It
// re-enqueues first so a crash cannot end the cycle.
func (rt *Runtime) retention(ctx context.Context) error {
	rt.scheduleRetention(ctx)
	n, err := rt.Store.DeleteExpired(ctx, rt.Cfg.Retention.Trace.D())
	if n > 0 {
		slog.Info("retention cleanup", "deleted", n)
	}
	rt.cleanupUploads(ctx, time.Now())
	return err
}

// cleanupUploads deletes bucket objects under uploads/ that are older than
// upload_ttl + 1h: upload slots whose turn never stored them. An error is
// logged, not fatal to the rest of the retention job.
func (rt *Runtime) cleanupUploads(ctx context.Context, now time.Time) {
	if rt.bucket == nil || !rt.Cfg.Files.Uploads() {
		return
	}
	before := now.Add(-rt.Cfg.Files.UploadTTL.D() - time.Hour)
	iter := rt.bucket.List(&blob.ListOptions{Prefix: "uploads/"})
	var deleted int
	for {
		obj, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			slog.Error("cleanup uploads", "err", err)
			return
		}
		if obj.ModTime.After(before) {
			continue
		}
		if err := rt.bucket.Delete(ctx, obj.Key); err != nil {
			slog.Error("cleanup uploads", "key", obj.Key, "err", err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		slog.Info("retention cleanup", "uploads", deleted)
	}
}
