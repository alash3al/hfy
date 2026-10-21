UPDATE grants SET revoked_at = coalesce(revoked_at, now()), expires_at = least(expires_at, now())
WHERE uuid = @uuid
RETURNING id
