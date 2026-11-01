SELECT id FROM conversation_events
WHERE conversation_id = @conversation AND type = 'turn'
ORDER BY id DESC
LIMIT 1
