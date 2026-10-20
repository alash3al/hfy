SELECT coalesce(sum((data->>'input')::bigint), 0)::bigint,
       coalesce(sum((data->>'output')::bigint), 0)::bigint,
       coalesce(sum((data->>'cached')::bigint), 0)::bigint,
       count(*)::bigint
FROM session_events
WHERE parent_id = @turn AND type = 'model.usage'
