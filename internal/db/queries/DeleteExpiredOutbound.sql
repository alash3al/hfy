DELETE FROM outbound_requests WHERE id IN (
    SELECT id FROM outbound_requests WHERE created_at < now() - @age::interval LIMIT @limit)
