package db

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateConversation(ctx context.Context) (ConversationRow, error) {
	return one[ConversationRow](s.Pool.Query(ctx, s.q("InsertConversation")))
}

func (s *Store) ConversationByUUID(ctx context.Context, id uuid.UUID) (ConversationRow, error) {
	return one[ConversationRow](s.Pool.Query(ctx, s.q("SelectConversationByUUID"), pgx.NamedArgs{"uuid": id}))
}

// LockConversation selects the conversation row FOR UPDATE inside tx. Posting, finishing
// a turn, approvals and stop all take this lock, so they serialize per conversation.
func (s *Store) LockConversation(ctx context.Context, tx pgx.Tx, id int64) (ConversationRow, error) {
	return one[ConversationRow](tx.Query(ctx, s.q("LockConversation"), pgx.NamedArgs{"id": id}))
}

func (s *Store) SetActiveTurn(ctx context.Context, q DBTX, conversation int64, turn *int64) error {
	_, err := q.Exec(ctx, s.q("SetActiveTurn"), pgx.NamedArgs{"id": conversation, "turn": turn})
	return err
}

func (s *Store) AddConversationUsage(ctx context.Context, q DBTX, conversation int64, in, out, cached int64) error {
	_, err := q.Exec(ctx, s.q("AddConversationUsage"),
		pgx.NamedArgs{"id": conversation, "input": in, "output": out, "cached": cached})
	return err
}

func (s *Store) ConversationByID(ctx context.Context, id int64) (ConversationRow, error) {
	return one[ConversationRow](s.Pool.Query(ctx, s.q("SelectConversationByID"), pgx.NamedArgs{"id": id}))
}

// SettleTurns advances the conversation's settled_until watermark to just before
// oldestOpen (nil: no turn is open, so up to the newest turn). Every turn up
// to the watermark has ended, so OpenTurns and QueuedTurns start after it. It
// never moves backwards. Call it under the conversation lock (svc does).
func (s *Store) SettleTurns(ctx context.Context, q DBTX, conversation int64, oldestOpen *int64) error {
	_, err := q.Exec(ctx, s.q("SettleTurns"), pgx.NamedArgs{"id": conversation, "oldest_open": oldestOpen})
	return err
}

// OpenTurns returns ids of turns that are not in a terminal state, oldest first.
func (s *Store) OpenTurns(ctx context.Context, q DBTX, conversation int64, limit int) ([]int64, error) {
	rows, err := q.Query(ctx, s.q("OpenTurns"), pgx.NamedArgs{"conversation": conversation, "limit": limit})
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}
