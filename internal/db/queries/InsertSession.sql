INSERT INTO sessions (agent) VALUES (@agent)
RETURNING id, uuid, agent, active_turn_id, summary_id, input_tokens, output_tokens, cached_tokens, created_at, updated_at
