UPDATE sessions SET settled_until = greatest(settled_until, coalesce(
    @oldest_open::bigint - 1,
    (SELECT max(id) FROM session_events WHERE session_id = @id AND type = 'turn'),
    0))
WHERE id = @id
