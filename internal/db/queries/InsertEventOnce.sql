WITH ins AS (
    INSERT INTO conversation_events (conversation_id, parent_id, actor_id, scope, type, data, v, ephemeral, request_id, key)
    VALUES (@conversation_id, @parent_id, @actor_id, @scope, @type, @data, @v, @ephemeral, @request_id, @key)
    ON CONFLICT (conversation_id, actor_id, key) WHERE key IS NOT NULL DO NOTHING
    RETURNING *
)
SELECT ins.id, ins.uuid, ins.conversation_id, ins.parent_id, p.uuid AS parent_uuid,
       ins.actor_id, a.uuid AS actor_uuid, a.type AS actor_type, a.ref AS actor_ref,
       ins.scope, ins.type, ins.data, ins.v, ins.ephemeral, ins.request_id, ins.created_at
FROM ins
JOIN actors a ON a.id = ins.actor_id
LEFT JOIN conversation_events p ON p.id = ins.parent_id
