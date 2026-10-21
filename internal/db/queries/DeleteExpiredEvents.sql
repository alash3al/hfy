DELETE FROM conversation_events WHERE id IN (
    SELECT id FROM conversation_events WHERE ephemeral AND created_at < now() - @age::interval LIMIT @limit)
