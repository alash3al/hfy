package db

import (
	"context"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// VectorLiteral renders a vector in pgvector's text format.
func VectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

type UnembeddedEvent struct {
	ID    int64   `db:"id"`
	Scope *string `db:"scope"`
	Text  string  `db:"text"`
}

// MissingEmbeddings returns message.text events after `after` that have no
// embedding for the configured model yet.
func (s *Store) MissingEmbeddings(ctx context.Context, after int64, limit int) ([]UnembeddedEvent, error) {
	return collect[UnembeddedEvent](s.Pool.Query(ctx, s.q("MissingEmbeddings"),
		pgx.NamedArgs{"after": after, "limit": limit, "model": s.emb.Model}))
}

// UnembeddedByIDs returns those of the given message.text events that have no
// embedding for the configured model yet.
func (s *Store) UnembeddedByIDs(ctx context.Context, ids []int64) ([]UnembeddedEvent, error) {
	return collect[UnembeddedEvent](s.Pool.Query(ctx, s.q("UnembeddedByIDs"),
		pgx.NamedArgs{"ids": ids, "model": s.emb.Model}))
}

func (s *Store) UpsertEmbedding(ctx context.Context, eventID int64, scope string, vec []float32) error {
	_, err := s.Pool.Exec(ctx, s.q("UpsertEmbedding"), pgx.NamedArgs{
		"id": eventID, "scope": scope, "model": s.emb.Model, "embedding": VectorLiteral(vec),
	})
	return err
}

type SearchHit struct {
	EventUUID   uuid.UUID `db:"event_uuid" json:"event"`
	SessionUUID uuid.UUID `db:"session_uuid" json:"session"`
	ActorType   string    `db:"actor_type" json:"actor_type"`
	ActorRef    string    `db:"actor_ref" json:"actor"`
	Text        string    `db:"text" json:"text"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	Dist        float64   `db:"dist" json:"distance"`
}

// SearchEmbeddings runs a scoped similarity search. HNSW iterative scans keep
// the scope filter from starving the result set.
func (s *Store) SearchEmbeddings(ctx context.Context, scope string, vec []float32, limit int) ([]SearchHit, error) {
	qvec := "@q::vector"
	if s.half() {
		qvec = "@q::vector::halfvec(" + strconv.Itoa(s.emb.Dims) + ")"
	}
	text := s.renderEmb(s.sql["SearchEmbeddings"], qvec)
	var hits []SearchHit
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL hnsw.iterative_scan = relaxed_order"); err != nil {
			return err
		}
		var err error
		hits, err = collect[SearchHit](tx.Query(ctx, text, pgx.NamedArgs{
			"q": VectorLiteral(vec), "scope": scope, "model": s.emb.Model, "limit": limit,
		}))
		return err
	})
	return hits, err
}
