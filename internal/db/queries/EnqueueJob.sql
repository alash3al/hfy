INSERT INTO jobs (type, queue, payload, run_at, max_attempts, concurrency_key)
SELECT @type, @queue, @payload, coalesce(@run_at::timestamptz, now()), @max_attempts, @key
WHERE @dedupe_states::text[] IS NULL
   OR NOT EXISTS (
       SELECT 1 FROM jobs
       WHERE type = @type AND state = ANY(@dedupe_states::text[])
         AND (NOT @dedupe_future::boolean OR run_at > now()))
RETURNING id
