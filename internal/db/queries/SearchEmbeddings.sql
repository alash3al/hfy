SELECT ev.uuid AS event_uuid, s.uuid AS session_uuid, a.type AS actor_type, a.ref AS actor_ref,
       ev.data->>'text' AS text, ev.created_at, x.dist
FROM (
    SELECT embeddable_id, {{embedding}} <=> {{qvec}} AS dist
    FROM {{table}}
    WHERE embeddable_type = 'session_event' AND scope = @scope AND model = @model
    ORDER BY {{embedding}} <=> {{qvec}}
    LIMIT @limit
) x
JOIN session_events ev ON ev.id = x.embeddable_id
JOIN sessions s ON s.id = ev.session_id
JOIN actors a ON a.id = ev.actor_id
ORDER BY x.dist
