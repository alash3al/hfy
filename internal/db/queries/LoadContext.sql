SELECT e.id, e.uuid, e.session_id, e.parent_id, p.uuid AS parent_uuid,
       e.actor_id, a.uuid AS actor_uuid, a.type AS actor_type, a.ref AS actor_ref,
       e.scope, e.type, e.data, e.ephemeral, e.request_id, e.created_at
FROM session_events e
JOIN actors a ON a.id = e.actor_id
LEFT JOIN session_events p ON p.id = e.parent_id
WHERE e.session_id = @session AND NOT e.ephemeral
  AND e.id > @after_turn
  AND coalesce(e.parent_id, e.id) > @after_turn
  AND coalesce(e.parent_id, e.id) <= @upto_turn
  AND e.type IN ('turn', 'message.text', 'tool_call', 'tool_call.interrupted', 'tool_result')
ORDER BY coalesce(e.parent_id, e.id), e.id
