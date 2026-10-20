SELECT e.data FROM session_events e
JOIN sessions s ON s.summary_id = e.id
WHERE s.id = @session
