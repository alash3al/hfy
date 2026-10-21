SELECT t.id FROM conversation_events t
WHERE t.conversation_id = @conversation AND t.type = 'turn'
  AND t.id > (SELECT settled_until FROM conversations WHERE id = @conversation)
  AND coalesce((SELECT s.data->>'state' FROM conversation_events s
                WHERE s.parent_id = t.id AND s.type = 'turn.state'
                ORDER BY s.id DESC LIMIT 1), 'queued')
      NOT IN ('completed', 'failed', 'stopped', 'cancelled')
ORDER BY t.id
LIMIT @limit
