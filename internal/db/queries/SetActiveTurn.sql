UPDATE conversations SET active_turn_id = @turn, updated_at = now() WHERE id = @id
