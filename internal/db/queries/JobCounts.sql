SELECT queue, state, count(*) FROM jobs WHERE state IN ('queued', 'running') GROUP BY queue, state
