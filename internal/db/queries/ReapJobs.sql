UPDATE jobs
SET state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
    lease_until = NULL,
    last_error = 'lease expired'
WHERE state = 'running' AND lease_until < now()
RETURNING id, type, queue, payload, state, run_at, attempts, max_attempts, lease_until, concurrency_key, last_error, created_at
