UPDATE grants SET revoked_at = coalesce(revoked_at, now()), expires_at = least(expires_at, now())
WHERE (root_id = @chain OR uuid = @chain) AND expires_at > now()
