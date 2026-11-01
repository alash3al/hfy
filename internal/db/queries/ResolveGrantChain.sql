SELECT coalesce(root_id, uuid) FROM grants WHERE root_id = @id OR uuid = @id LIMIT 1
