WITH hit AS (SELECT root_id FROM grants WHERE root_id = @id OR uuid = @id LIMIT 1),
     ended AS (
    UPDATE grants SET revoked_at = coalesce(revoked_at, now()), expires_at = least(expires_at, now())
    WHERE root_id = (SELECT root_id FROM hit) AND expires_at > now()
)
SELECT root_id FROM hit
