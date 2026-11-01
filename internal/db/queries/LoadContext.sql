-- A note or host message belongs to the turn that was newest when it was
-- appended (its place in event order), whatever turn it names as its parent:
-- events appended after a summary are then never lost to a covered turn.
-- That turn is reported as parent_id.
SELECT e.id, e.uuid, e.conversation_id, e.turn_id AS parent_id, p.uuid AS parent_uuid,
       e.actor_id, a.uuid AS actor_uuid, a.type AS actor_type, a.ref AS actor_ref,
       e.scope, e.type, e.data, e.v, e.ephemeral, e.request_id, e.created_at
FROM (
    SELECT x.*,
           CASE WHEN x.type IN ('context.note', 'host.message')
                THEN coalesce((SELECT max(t.id) FROM conversation_events t
                               WHERE t.conversation_id = x.conversation_id AND t.type = 'turn' AND t.id < x.id),
                              x.parent_id)
                ELSE x.parent_id END AS turn_id
    FROM conversation_events x
    WHERE x.conversation_id = @conversation AND NOT x.ephemeral AND x.id > @after_turn
      AND x.type IN ('turn', 'message.text', 'tool_call', 'tool_call.interrupted', 'tool_result',
                     'context.note', 'host.message')
) e
JOIN actors a ON a.id = e.actor_id
LEFT JOIN conversation_events p ON p.id = e.turn_id
WHERE coalesce(e.turn_id, e.id) > @after_turn
  AND coalesce(e.turn_id, e.id) <= @upto_turn
ORDER BY coalesce(e.turn_id, e.id), e.id
