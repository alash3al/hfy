SELECT t.id FROM session_events t
WHERE t.session_id = @session AND t.type = 'turn' AND t.id > @after
  AND t.id > (SELECT settled_until FROM sessions WHERE id = @session)
  AND (SELECT s.data->>'state' FROM session_events s
       WHERE s.parent_id = t.id AND s.type = 'turn.state'
       ORDER BY s.id DESC LIMIT 1) = 'queued'
ORDER BY t.id
