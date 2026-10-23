package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
	w.Handle(types.JobEmbedEvents, func(ctx context.Context, p svc.EmbedPayload, _ db.JobRow) error {
		return rt.embedEvents(ctx, p.EventIDs)
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
// then re-enqueues itself. It re-enqueues first so a crash cannot end the cycle.
func (rt *Runtime) retention(ctx context.Context) error {
	rt.scheduleRetention(ctx)
	n, err := rt.Store.DeleteExpired(ctx, rt.Cfg.Retention.Trace.D())
	if n > 0 {
		slog.Info("retention cleanup", "deleted", n)
	}
	return err
}

// embedEvents embeds the given message.text events, or with none, backfills
// every message.text event that has no vector for the configured model yet.
func (rt *Runtime) embedEvents(ctx context.Context, ids []int64) error {
	if rt.embedder == nil {
		return nil
	}
	if len(ids) > 0 {
		batch, err := rt.Store.UnembeddedByIDs(ctx, ids)
		if err != nil {
			return err
		}
		return rt.embedBatch(ctx, batch)
	}
	var after int64
	for {
		batch, err := rt.Store.MissingEmbeddings(ctx, after, 32)
		if err != nil || len(batch) == 0 {
			return err
		}
		if err := rt.embedBatch(ctx, batch); err != nil {
			return err
		}
		after = batch[len(batch)-1].ID
	}
}

func (rt *Runtime) embedBatch(ctx context.Context, batch []db.UnembeddedEvent) error {
	if len(batch) == 0 {
		return nil
	}
	dims := rt.Cfg.Embeddings.Dimensions
	rec := rt.recorder(nil, types.OutboundEmbedding)
	texts := make([]string, len(batch))
	for i, b := range batch {
		texts[i] = truncate(b.Text, maxEmbedChars)
	}
	vecs, err := rt.embedder.Embed(ctx, rt.embModel, texts, rec)
	if err != nil {
		return err
	}
	for i, b := range batch {
		if len(vecs[i]) != dims {
			return fmt.Errorf("embedding model returned %d dimensions, embeddings.dimensions is %d", len(vecs[i]), dims)
		}
		if err := rt.Store.UpsertEmbedding(ctx, b.ID, b.Scope, vecs[i]); err != nil {
			return err
		}
	}
	return nil
}
