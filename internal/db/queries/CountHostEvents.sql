SELECT count(*) FROM conversation_events e
JOIN actors a ON a.id = e.actor_id
WHERE e.parent_id = @turn AND a.type = 'host' AND e.type IN ('context.note', 'host.message')
