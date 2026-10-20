INSERT INTO actors (type, ref) VALUES (@type, @ref)
ON CONFLICT (type, ref) DO NOTHING
RETURNING id
