package db

// SQL returns an embedded query by name (rendered for the embeddings table).
// Tests use it to EXPLAIN the hot queries.
func (s *Store) SQL(name string) string { return s.q(name) }
