package db_test

import (
	"context"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/types"
)

func renewableGrant(t *testing.T, s *db.Store, hash string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := s.FindOrCreateActor(ctx, s.Pool, types.ActorExternal, "u1")
	id, _, err := s.CreateGrant(ctx, db.NewGrant{TokenHash: []byte(hash), ConversationID: conv.ID, ActorID: actor,
		Scope: "u1", ExpiresIn: time.Minute, MaxLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The exchange is one statement: when its insert fails (here the new hash is
// taken), the old grant is exactly as it was, not retired.
func TestFailedExchangeLeavesTheOldGrantAlone(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	id := renewableGrant(t, s, "old")
	renewableGrant(t, s, "taken")
	var before, after time.Time
	s.Pool.QueryRow(ctx, "SELECT expires_at FROM grants WHERE uuid = $1", id).Scan(&before)

	_, err := s.ExchangeGrant(ctx, []byte("old"), []byte("taken"), 30*time.Second, 0)
	if !db.IsUniqueViolation(err) {
		t.Fatalf("want a unique violation, got %v", err)
	}
	var revoked bool
	s.Pool.QueryRow(ctx, "SELECT revoked_at IS NOT NULL, expires_at FROM grants WHERE uuid = $1", id).Scan(&revoked, &after)
	if revoked || !after.Equal(before) {
		t.Fatalf("the old grant changed: revoked %v, expires %v -> %v", revoked, before, after)
	}
	var n int
	s.Pool.QueryRow(ctx, "SELECT count(*) FROM grants").Scan(&n)
	if n != 2 {
		t.Fatalf("%d grants", n)
	}
	if _, err := s.ExchangeGrant(ctx, []byte("old"), []byte("fresh"), 30*time.Second, 0); err != nil {
		t.Fatalf("the grant must still exchange: %v", err)
	}
}

// Every grant has a unique uuid: a colliding one is refused, never merged into a chain.
func TestGrantUUIDsAreUnique(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	id := renewableGrant(t, s, "a")
	_, err := s.Pool.Exec(ctx, `INSERT INTO grants (uuid, root_id, token_hash, conversation_id, actor_id, scope, agent, expires_at)
		SELECT uuid, root_id, 'b', conversation_id, actor_id, scope, agent, expires_at FROM grants WHERE uuid = $1`, id)
	if !db.IsUniqueViolation(err) {
		t.Fatalf("want a unique violation, got %v", err)
	}
}

// Retention deletes the retired rows of a chain without touching the live one,
// and the chain is still revoked by its id after its first row is gone: no
// foreign key, and revoke names the chain, not a row.
func TestRetentionKeepsALiveChainRevocable(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	id := renewableGrant(t, s, "t0")
	for _, h := range []string{"t1", "t2", "t3"} {
		prev := map[string]string{"t1": "t0", "t2": "t1", "t3": "t2"}[h]
		if _, err := s.ExchangeGrant(ctx, []byte(prev), []byte(h), 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE grants SET expires_at = now() - interval '31 days' WHERE token_hash <> 't3'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteExpired(ctx, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	s.Pool.QueryRow(ctx, "SELECT count(*) FROM grants").Scan(&n)
	if n != 1 {
		t.Fatalf("%d rows left, want the live head", n)
	}
	var root uuid.UUID
	if err := s.Pool.QueryRow(ctx, "SELECT root_id FROM grants WHERE token_hash = 't3'").Scan(&root); err != nil || root != id {
		t.Fatalf("the head after retention: %v %v", root, err)
	}
	if err := s.RevokeGrant(ctx, id); err != nil {
		t.Fatalf("revoke after the first row was deleted: %v", err)
	}
	if _, err := s.GrantByToken(ctx, []byte("t3")); !db.IsNotFound(err) {
		t.Fatalf("the head survived the revoke: %v", err)
	}
	var live int
	s.Pool.QueryRow(ctx, "SELECT count(*) FROM grants WHERE expires_at > now()").Scan(&live)
	if live != 0 {
		t.Fatalf("%d live rows", live)
	}
}

// Exchanges and retention at the same time: no errors, one live head throughout.
func TestExchangeWhileRetentionRuns(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	id := renewableGrant(t, s, "c0")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			// age 0: everything that has ended is deleted, grace rows included once they end
			if _, err := s.DeleteExpired(ctx, 0); err != nil {
				t.Errorf("retention: %v", err)
				return
			}
		}
	})
	prev := "c0"
	for i := 1; i <= 60; i++ {
		next := "c" + string(rune('a'+i%26)) + string(rune('A'+i/26))
		if _, err := s.ExchangeGrant(ctx, []byte(prev), []byte(next), 0, 0); err != nil {
			t.Fatalf("exchange %d: %v", i, err)
		}
		prev = next
	}
	close(stop)
	wg.Wait()
	var live int
	s.Pool.QueryRow(ctx, "SELECT count(*) FROM grants WHERE root_id = $1 AND expires_at > now()", id).Scan(&live)
	if live != 1 {
		t.Fatalf("%d live rows, want the head", live)
	}
}
