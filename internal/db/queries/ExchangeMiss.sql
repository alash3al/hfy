SELECT revoked_at IS NOT NULL AS retired,
       coalesce(expires_at >= renew_until, false) AS capped,
       coalesce(greatest(ceil(extract(epoch FROM created_at + make_interval(secs => renew_secs * @gap::float8) - now())), 0), 0)::int AS wait
FROM grants WHERE token_hash = @token_hash AND expires_at > now()
