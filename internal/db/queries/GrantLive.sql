SELECT EXISTS (SELECT 1 FROM grants WHERE id = @id AND expires_at > now())
