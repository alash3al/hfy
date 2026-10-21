SELECT data FROM conversation_events
WHERE parent_id = @turn AND type = 'turn.state'
ORDER BY id DESC
LIMIT 1
