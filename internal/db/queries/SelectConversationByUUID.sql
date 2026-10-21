SELECT id, uuid, active_turn_id, summary_id, input_tokens, output_tokens, cached_tokens, created_at, updated_at
FROM conversations WHERE uuid = @uuid
