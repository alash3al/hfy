SELECT e.data FROM conversation_events e
JOIN conversations s ON s.summary_id = e.id
WHERE s.id = @conversation
