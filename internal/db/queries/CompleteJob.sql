UPDATE jobs SET state = 'done', lease_until = NULL
WHERE id = @id AND state = 'running' AND attempts = @attempt
