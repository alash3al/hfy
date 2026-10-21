INSERT INTO grants (token_hash, conversation_id, actor_id, scope, agent, vars, expires_at)
VALUES (@token_hash, @conversation, @actor, @scope, @agent, @vars, now() + make_interval(secs => @expires_in::int))
RETURNING uuid, expires_at
