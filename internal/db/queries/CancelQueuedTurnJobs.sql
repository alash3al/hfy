DELETE FROM jobs WHERE state = 'queued' AND type = 'run_turn' AND concurrency_key = @key
