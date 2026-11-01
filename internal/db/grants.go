package db

import (
	"context"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// NewGrant is a client's access to one conversation as one actor. The token
// itself is never stored, only its hash. A MaxLifetime above zero makes the
// grant renewable: ExchangeGrant swaps its token for a new one, each living
// ExpiresIn, until MaxLifetime after now.
type NewGrant struct {
	TokenHash      []byte
	ConversationID int64
	ActorID        int64
	Scope          string
	Agent          string
	Vars           map[string]string
	ExpiresIn      time.Duration
	MaxLifetime    time.Duration
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
	Renewable        bool              `db:"renewable"`
}

func (s *Store) CreateGrant(ctx context.Context, g NewGrant) (id uuid.UUID, expiresAt time.Time, err error) {
	vars := g.Vars
	if vars == nil {
		vars = map[string]string{}
	}
	var maxLifetime *int
	if g.MaxLifetime > 0 {
		maxLifetime = new(int(g.MaxLifetime.Seconds()))
	}
	id = uuid.New() // the chain's id too: the first row names its chain
	err = s.Pool.QueryRow(ctx, s.q("InsertGrant"), pgx.NamedArgs{
		"uuid": id, "token_hash": g.TokenHash, "conversation": g.ConversationID, "actor": g.ActorID, "scope": g.Scope,
		"agent": g.Agent, "vars": vars, "expires_in": int(g.ExpiresIn.Seconds()), "max_lifetime": maxLifetime,
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

// Exchanged is the grant that replaced another: ChainID is the id the backend
// stored, ExpiresAt the new token's end, Renewable whether it can be
// exchanged once more (false at the grant's cap).
type Exchanged struct {
	ChainID   uuid.UUID
	ExpiresAt time.Time
	Renewable bool
}

// ExchangeGrant retires the live renewable grant behind oldHash and creates its
// successor with newHash, in one statement, so exactly one of any number of
// concurrent exchanges wins and a failed insert leaves the old grant as it was.
// The old token keeps working for grace. No match is pgx.ErrNoRows.
//
// The chain lock orders it against RevokeGrant: without it a revoke that began
// before the exchange committed would not see the successor and leave it live.
func (s *Store) ExchangeGrant(ctx context.Context, oldHash, newHash []byte, grace time.Duration) (Exchanged, error) {
	var out Exchanged
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, s.q("LockGrantChainOfToken"), pgx.NamedArgs{"token_hash": oldHash}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, s.q("ExchangeGrant"), pgx.NamedArgs{
			"old": oldHash, "new": newHash, "grace": grace.Seconds(),
		}).Scan(&out.ChainID, &out.ExpiresAt, &out.Renewable)
	})
	return out, err
}

// RevokeGrant ends a grant now, and every grant its exchanges made; revoking
// it again changes nothing. id is the one CreateGrant returned. An unknown id
// is pgx.ErrNoRows.
func (s *Store) RevokeGrant(ctx context.Context, id uuid.UUID) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, s.q("LockGrantChain"), pgx.NamedArgs{"chain": id}); err != nil {
			return err
		}
		var chain uuid.UUID
		return tx.QueryRow(ctx, s.q("RevokeGrant"), pgx.NamedArgs{"id": id}).Scan(&chain)
	})
}
