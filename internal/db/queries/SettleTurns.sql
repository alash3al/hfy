UPDATE conversations SET settled_until = greatest(settled_until, coalesce(
    @oldest_open::bigint - 1,
    (SELECT max(id) FROM conversation_events WHERE conversation_id = @id AND type = 'turn'),
    0))
WHERE id = @id
