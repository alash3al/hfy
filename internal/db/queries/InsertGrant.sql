INSERT INTO grants (uuid, root_id, token_hash, conversation_id, actor_id, scope, agent, vars, expires_at, renew_until, renew_secs)
VALUES (@uuid, @uuid, @token_hash, @conversation, @actor, @scope, @agent, @vars, now() + make_interval(secs => @expires_in::int),
        CASE WHEN @max_lifetime::int IS NULL THEN NULL ELSE now() + make_interval(secs => @max_lifetime::int) END,
        CASE WHEN @max_lifetime::int IS NULL THEN NULL ELSE @expires_in::int END)
RETURNING uuid, expires_at
