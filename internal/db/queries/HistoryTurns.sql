SELECT id FROM conversation_events
WHERE conversation_id = @conversation AND type = 'turn' AND (@before::bigint IS NULL OR id < @before)
ORDER BY id DESC
LIMIT @limit
