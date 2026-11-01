SELECT pg_advisory_xact_lock(hashtextextended(root_id::text, 0)) FROM grants WHERE token_hash = @token_hash
