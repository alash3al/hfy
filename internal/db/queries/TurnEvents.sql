SELECT e.id, e.uuid, e.session_id, e.parent_id, p.uuid AS parent_uuid,
       e.actor_id, a.uuid AS actor_uuid, a.type AS actor_type, a.ref AS actor_ref,
       e.scope, e.type, e.data, e.v, e.ephemeral, e.request_id, e.created_at
FROM session_events e
JOIN actors a ON a.id = e.actor_id
LEFT JOIN session_events p ON p.id = e.parent_id
WHERE NOT e.ephemeral AND (e.id = @turn OR e.parent_id = @turn)
ORDER BY e.id
