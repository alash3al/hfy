package db

import (
	"context"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// NewGrant is a client's access to one conversation as one actor. The token
// itself is never stored, only its hash.
type NewGrant struct {
	TokenHash      []byte
	ConversationID int64
	ActorID        int64
	Scope          string
	Agent          string
	Vars           map[string]string
	ExpiresIn      time.Duration
}

// GrantRow is a valid grant as a client request sees it.
type GrantRow struct {
	ID               int64             `db:"id"`
	ConversationID   int64             `db:"conversation_id"`
	ConversationUUID uuid.UUID         `db:"conversation_uuid"`
	ActorID          int64             `db:"actor_id"`
	ActorUUID        uuid.UUID         `db:"actor_uuid"`
	ActorType        string            `db:"actor_type"`
	ActorRef         string            `db:"actor_ref"`
	Scope            string            `db:"scope"`
	Agent            string            `db:"agent"`
	Vars             map[string]string `db:"vars"`
	ExpiresAt        time.Time         `db:"expires_at"`
}

func (s *Store) CreateGrant(ctx context.Context, g NewGrant) (id uuid.UUID, expiresAt time.Time, err error) {
	vars := g.Vars
	if vars == nil {
		vars = map[string]string{}
	}
	err = s.Pool.QueryRow(ctx, s.q("InsertGrant"), pgx.NamedArgs{
		"token_hash": g.TokenHash, "conversation": g.ConversationID, "actor": g.ActorID, "scope": g.Scope,
		"agent": g.Agent, "vars": vars, "expires_in": int(g.ExpiresIn.Seconds()),
	}).Scan(&id, &expiresAt)
	return id, expiresAt, err
}

// GrantByToken returns the grant whose token hashes to hash, if it has
// neither expired nor been revoked.
func (s *Store) GrantByToken(ctx context.Context, hash []byte) (GrantRow, error) {
	return one[GrantRow](s.Pool.Query(ctx, s.q("SelectGrantByToken"), pgx.NamedArgs{"token_hash": hash}))
}

// GrantLive reports whether a grant still holds: open streams ask it.
func (s *Store) GrantLive(ctx context.Context, id int64) (bool, error) {
	var live bool
	err := s.Pool.QueryRow(ctx, s.q("GrantLive"), pgx.NamedArgs{"id": id}).Scan(&live)
	return live, err
}

// RevokeGrant ends a grant now; revoking it again changes nothing. An
// unknown grant is pgx.ErrNoRows.
func (s *Store) RevokeGrant(ctx context.Context, id uuid.UUID) error {
	var internal int64
	return s.Pool.QueryRow(ctx, s.q("RevokeGrant"), pgx.NamedArgs{"uuid": id}).Scan(&internal)
}
