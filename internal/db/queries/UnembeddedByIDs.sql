SELECT e.id, e.scope, e.data->>'text' AS text
FROM session_events e
WHERE e.id = ANY(@ids) AND e.type = 'message.text' AND coalesce(e.data->>'text', '') <> ''
  AND NOT EXISTS (
      SELECT 1 FROM {{table}} m
      WHERE m.embeddable_type = 'session_event' AND m.embeddable_id = e.id AND m.model = @model)
ORDER BY e.id
