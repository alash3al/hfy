DELETE FROM session_events WHERE id IN (
    SELECT id FROM session_events WHERE ephemeral AND created_at < now() - @age::interval LIMIT @limit)
