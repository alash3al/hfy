SELECT e.id, e.scope, e.data->>'text' AS text
FROM conversation_events e
WHERE e.type = 'message.text' AND e.id > @after AND coalesce(e.data->>'text', '') <> ''
  AND NOT EXISTS (
      SELECT 1 FROM {{table}} m
      WHERE m.embeddable_type = 'conversation_event' AND m.embeddable_id = e.id AND m.model = @model)
ORDER BY e.id
LIMIT @limit
