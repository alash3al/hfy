SELECT t.id FROM session_events t
WHERE t.session_id = @session AND t.type = 'turn'
  AND coalesce((SELECT s.data->>'state' FROM session_events s
                WHERE s.parent_id = t.id AND s.type = 'turn.state'
                ORDER BY s.id DESC LIMIT 1), 'queued')
      NOT IN ('completed', 'failed', 'stopped', 'cancelled')
ORDER BY t.id
LIMIT @limit
