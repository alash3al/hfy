SELECT coalesce(max(id), 0) FROM session_events WHERE session_id = @session
