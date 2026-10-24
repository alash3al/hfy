SELECT g.id, g.conversation_id, c.uuid AS conversation_uuid, g.actor_id,
       a.uuid AS actor_uuid, a.type AS actor_type, a.ref AS actor_ref,
       g.scope, g.agent, g.vars, g.expires_at
FROM grants g
JOIN conversations c ON c.id = g.conversation_id
JOIN actors a ON a.id = g.actor_id
WHERE g.token_hash = @token_hash AND g.expires_at > now()
