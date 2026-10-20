package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/types"
)

// NewJob describes a job to enqueue.
type NewJob struct {
	Type        types.JobType
	Queue       string
	Payload     any
	RunAt       *time.Time
	MaxAttempts int
	Key         string // concurrency key; "" = no limit
	// DedupeStates, when set, skips the insert if a job of the same type is
	// already in one of these states.
	DedupeStates []types.JobState
	// DedupeFuture narrows the dedupe to jobs that have not become runnable
	// yet. Only such a job is guaranteed to start after the caller commits.
	DedupeFuture bool
}

// SessionKey is the concurrency key that serializes a session's jobs.
func SessionKey(session int64) string { return "session:" + itoa(session) }

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// Enqueue inserts a job and wakes workers. It returns false when deduped.
func (s *Store) Enqueue(ctx context.Context, q DBTX, j NewJob) (bool, error) {
	payload, err := json.Marshal(j.Payload)
	if err != nil {
		return false, err
	}
	var key, states any
	if j.Key != "" {
		key = j.Key
	}
	if len(j.DedupeStates) > 0 {
		ss := make([]string, len(j.DedupeStates))
		for i, st := range j.DedupeStates {
			ss[i] = string(st)
		}
		states = ss
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 3
	}
	var id int64
	err = q.QueryRow(ctx, s.q("EnqueueJob"), pgx.NamedArgs{
		"type": string(j.Type), "queue": j.Queue, "payload": payload, "run_at": j.RunAt,
		"max_attempts": j.MaxAttempts, "key": key, "dedupe_states": states,
		"dedupe_future": j.DedupeFuture,
	}).Scan(&id)
	if IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = q.Exec(ctx, "SELECT pg_notify($1, $2)", ChanJobs, j.Queue)
	return true, err
}

// PendingTurnJob reports whether a run_turn job for the session is in one of
// the given states.
func (s *Store) PendingTurnJob(ctx context.Context, q DBTX, session int64, states ...types.JobState) (bool, error) {
	ss := make([]string, len(states))
	for i, st := range states {
		ss[i] = string(st)
	}
	var ok bool
	err := q.QueryRow(ctx, s.q("PendingTurnJob"), pgx.NamedArgs{"key": SessionKey(session), "states": ss}).Scan(&ok)
	return ok, err
}

func (s *Store) CancelQueuedTurnJobs(ctx context.Context, q DBTX, session int64) error {
	_, err := q.Exec(ctx, s.q("CancelQueuedTurnJobs"), pgx.NamedArgs{"key": SessionKey(session)})
	return err
}

func interval(d time.Duration) string { return d.String() }

// ClaimJob takes the next runnable job from a queue, or nil if none. Losing the
// concurrency_key race (unique violation) also returns nil: the caller moves on.
func (s *Store) ClaimJob(ctx context.Context, queue string, lease time.Duration) (*JobRow, error) {
	j, err := one[JobRow](s.Pool.Query(ctx, s.q("ClaimJob"),
		pgx.NamedArgs{"queue": queue, "lease": interval(lease)}))
	if IsNotFound(err) || IsUniqueViolation(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *Store) HeartbeatJob(ctx context.Context, id int64, lease time.Duration) error {
	_, err := s.Pool.Exec(ctx, s.q("HeartbeatJob"), pgx.NamedArgs{"id": id, "lease": interval(lease)})
	return err
}

func (s *Store) CompleteJob(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, s.q("CompleteJob"), pgx.NamedArgs{"id": id})
	return err
}

// RetryJob re-queues a failed job after delay, or marks it failed when attempts
// are exhausted. It returns the new state.
func (s *Store) RetryJob(ctx context.Context, id int64, delay time.Duration, errText string) (types.JobState, error) {
	var st string
	err := s.Pool.QueryRow(ctx, s.q("RetryJob"),
		pgx.NamedArgs{"id": id, "delay": interval(delay), "error": errText}).Scan(&st)
	return types.JobState(st), err
}

// ReapJobs requeues running jobs whose lease expired (failing those out of
// attempts) and returns them with their new state.
func (s *Store) ReapJobs(ctx context.Context) ([]JobRow, error) {
	return collect[JobRow](s.Pool.Query(ctx, s.q("ReapJobs")))
}

// DeleteExpired removes ephemeral events, outbound requests and finished jobs
// older than age, in batches. It returns the number of rows deleted.
func (s *Store) DeleteExpired(ctx context.Context, age time.Duration) (int64, error) {
	var total int64
	for _, name := range []string{"DeleteExpiredEvents", "DeleteExpiredOutbound", "PruneJobs"} {
		for {
			tag, err := s.Pool.Exec(ctx, s.q(name),
				pgx.NamedArgs{"age": interval(age), "limit": 5000})
			if err != nil {
				return total, err
			}
			total += tag.RowsAffected()
			if tag.RowsAffected() < 5000 {
				break
			}
		}
	}
	return total, nil
}
