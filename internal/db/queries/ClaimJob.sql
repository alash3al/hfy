UPDATE jobs SET state = 'running', attempts = attempts + 1, lease_until = now() + @lease::interval
WHERE id = (
    SELECT j.id FROM jobs j
    WHERE j.state = 'queued' AND j.queue = @queue AND j.run_at <= now()
      AND NOT EXISTS (
          SELECT 1 FROM jobs r
          WHERE r.concurrency_key = j.concurrency_key AND r.state = 'running')
    ORDER BY j.run_at, j.id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id, type, queue, payload, state, run_at, attempts, max_attempts, lease_until, concurrency_key, last_error, created_at
