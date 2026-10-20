package db

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateSession(ctx context.Context) (SessionRow, error) {
	return one[SessionRow](s.Pool.Query(ctx, s.q("InsertSession")))
}

func (s *Store) SessionByUUID(ctx context.Context, id uuid.UUID) (SessionRow, error) {
	return one[SessionRow](s.Pool.Query(ctx, s.q("SelectSessionByUUID"), pgx.NamedArgs{"uuid": id}))
}

// LockSession selects the session row FOR UPDATE inside tx. Posting, finishing
// a turn, approvals and stop all take this lock, so they serialize per session.
func (s *Store) LockSession(ctx context.Context, tx pgx.Tx, id int64) (SessionRow, error) {
	return one[SessionRow](tx.Query(ctx, s.q("LockSession"), pgx.NamedArgs{"id": id}))
}

func (s *Store) SetActiveTurn(ctx context.Context, q DBTX, session int64, turn *int64) error {
	_, err := q.Exec(ctx, s.q("SetActiveTurn"), pgx.NamedArgs{"id": session, "turn": turn})
	return err
}

func (s *Store) AddSessionUsage(ctx context.Context, q DBTX, session int64, in, out, cached int64) error {
	_, err := q.Exec(ctx, s.q("AddSessionUsage"),
		pgx.NamedArgs{"id": session, "input": in, "output": out, "cached": cached})
	return err
}
