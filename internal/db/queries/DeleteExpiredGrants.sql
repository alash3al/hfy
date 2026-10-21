DELETE FROM grants WHERE id IN (
    SELECT id FROM grants WHERE expires_at < now() - @age::interval LIMIT @limit)
