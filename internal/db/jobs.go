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
}

// ConversationKey is the concurrency key that serializes a conversation's jobs.
func ConversationKey(conversation int64) string { return "conversation:" + itoa(conversation) }

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

// PendingTurnJob reports whether a run_turn job for the conversation is in one of
// the given states.
func (s *Store) PendingTurnJob(ctx context.Context, q DBTX, conversation int64, states ...types.JobState) (bool, error) {
	ss := make([]string, len(states))
	for i, st := range states {
		ss[i] = string(st)
	}
	var ok bool
	err := q.QueryRow(ctx, s.q("PendingTurnJob"), pgx.NamedArgs{"key": ConversationKey(conversation), "states": ss}).Scan(&ok)
	return ok, err
}

func (s *Store) CancelQueuedTurnJobs(ctx context.Context, q DBTX, conversation int64) error {
	_, err := q.Exec(ctx, s.q("CancelQueuedTurnJobs"), pgx.NamedArgs{"key": ConversationKey(conversation)})
	return err
}

func interval(d time.Duration) string { return d.String() }

// ClaimJob takes the next runnable job of the given types from a queue, or nil
// if none. A worker claims only the types it handles, so during a rolling
// deploy an older worker leaves a newer job type to a newer worker. Losing
// the concurrency_key race (unique violation) also returns nil.
func (s *Store) ClaimJob(ctx context.Context, queue string, jobTypes []types.JobType, lease time.Duration) (*JobRow, error) {
	names := make([]string, len(jobTypes))
	for i, t := range jobTypes {
		names[i] = string(t)
	}
	j, err := one[JobRow](s.Pool.Query(ctx, s.q("ClaimJob"),
		pgx.NamedArgs{"queue": queue, "types": names, "lease": interval(lease)}))
	if IsNotFound(err) || IsUniqueViolation(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// The attempt number fences a job's owner: each claim increments it, so a
// worker whose lease expired and whose job was claimed again can no longer
// extend, complete, retry or release it.

// HeartbeatJob extends the lease. It returns false when the caller no longer
// owns the job (its lease expired and the job was reaped or claimed again).
func (s *Store) HeartbeatJob(ctx context.Context, id int64, attempt int, lease time.Duration) (bool, error) {
	tag, err := s.Pool.Exec(ctx, s.q("HeartbeatJob"), pgx.NamedArgs{"id": id, "attempt": attempt, "lease": interval(lease)})
	return tag.RowsAffected() == 1, err
}

func (s *Store) CompleteJob(ctx context.Context, id int64, attempt int) error {
	_, err := s.Pool.Exec(ctx, s.q("CompleteJob"), pgx.NamedArgs{"id": id, "attempt": attempt})
	return err
}

// RetryJob re-queues a failed job after delay, or marks it failed when attempts
// are exhausted. It returns the new state, or a not-found error when the
// caller no longer owns the job.
func (s *Store) RetryJob(ctx context.Context, id int64, attempt int, delay time.Duration, errText string) (types.JobState, error) {
	var st string
	err := s.Pool.QueryRow(ctx, s.q("RetryJob"),
		pgx.NamedArgs{"id": id, "attempt": attempt, "delay": interval(delay), "error": errText}).Scan(&st)
	return types.JobState(st), err
}

// ReleaseJob hands a job interrupted by shutdown back to the queue at once,
// instead of waiting for its lease to expire. A job on its last attempt is
// left to the reaper, which fails it.
func (s *Store) ReleaseJob(ctx context.Context, id int64, attempt int) error {
	_, err := s.Pool.Exec(ctx, s.q("ReleaseJob"), pgx.NamedArgs{"id": id, "attempt": attempt})
	return err
}

// ReapJobs requeues running jobs whose lease expired (failing those out of
// attempts) and returns them with their new state.
func (s *Store) ReapJobs(ctx context.Context) ([]JobRow, error) {
	return collect[JobRow](s.Pool.Query(ctx, s.q("ReapJobs")))
}

// DeleteExpired removes ephemeral events, outbound requests, ended grants and
// finished jobs older than age, in batches. It returns the number of rows
// deleted.
func (s *Store) DeleteExpired(ctx context.Context, age time.Duration) (int64, error) {
	var total int64
	for _, name := range []string{"DeleteExpiredEvents", "DeleteExpiredOutbound", "DeleteExpiredGrants", "PruneJobs"} {
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

// JobCounts returns the number of queued and running jobs by (queue, state).
func (s *Store) JobCounts(ctx context.Context) (map[[2]string]int64, error) {
	rows, err := s.Pool.Query(ctx, s.q("JobCounts"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]int64{}
	for rows.Next() {
		var queue, state string
		var n int64
		if err := rows.Scan(&queue, &state, &n); err != nil {
			return nil, err
		}
		out[[2]string{queue, state}] = n
	}
	return out, rows.Err()
}
