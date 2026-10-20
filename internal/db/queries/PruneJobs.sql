DELETE FROM jobs WHERE id IN (
    SELECT id FROM jobs WHERE state IN ('done', 'failed') AND created_at < now() - @age::interval LIMIT @limit)
