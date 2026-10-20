SELECT EXISTS (
    SELECT 1 FROM jobs
    WHERE type = 'run_turn' AND concurrency_key = @key AND state = ANY(@states::text[]))
