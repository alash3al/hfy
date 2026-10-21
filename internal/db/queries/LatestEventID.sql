SELECT coalesce(max(id), 0) FROM conversation_events WHERE conversation_id = @conversation
