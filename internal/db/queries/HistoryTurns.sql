SELECT id FROM session_events
WHERE session_id = @session AND type = 'turn' AND (@before::bigint IS NULL OR id < @before)
ORDER BY id DESC
LIMIT @limit
