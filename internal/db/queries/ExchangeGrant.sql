WITH old AS (
    UPDATE grants SET revoked_at = now(), expires_at = least(expires_at, now() + make_interval(secs => @grace::float8))
    WHERE token_hash = @old AND renew_until IS NOT NULL AND revoked_at IS NULL AND expires_at > now()
      AND expires_at < renew_until
      AND created_at <= now() - make_interval(secs => renew_secs * @gap::float8)
    RETURNING conversation_id, actor_id, scope, agent, vars, renew_until, renew_secs, root_id
)
INSERT INTO grants (token_hash, conversation_id, actor_id, scope, agent, vars, renew_until, renew_secs, root_id, expires_at)
SELECT @new, conversation_id, actor_id, scope, agent, vars, renew_until, renew_secs, root_id,
       least(renew_until, now() + make_interval(secs => renew_secs))
FROM old
RETURNING root_id, expires_at, expires_at < renew_until AS renewable
