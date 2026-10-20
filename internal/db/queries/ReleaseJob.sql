UPDATE jobs SET state = 'queued', lease_until = NULL, run_at = now()
WHERE id = @id AND state = 'running' AND attempts = @attempt AND attempts < max_attempts
