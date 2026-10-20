UPDATE jobs
SET state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
    run_at = now() + @delay::interval,
    lease_until = NULL,
    last_error = @error
WHERE id = @id
RETURNING state
