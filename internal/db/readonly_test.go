package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
)

// A read-only store cannot write, whatever the code does: Postgres refuses.
func TestOpenReadOnlyRefusesWrites(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	ro, err := db.OpenReadOnly(ctx, s.Pool.Config().ConnString(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var n int
	if err := ro.Pool.QueryRow(ctx, `SELECT count(*) FROM actors`).Scan(&n); err != nil {
		t.Fatalf("reads must work: %v", err)
	}
	_, err = ro.FindOrCreateActor(ctx, ro.Pool, "external", "x")
	if err == nil || !strings.Contains(err.Error(), "read-only transaction") {
		t.Fatalf("a write through a read-only store: %v", err)
	}
	if _, err := ro.CreateConversation(ctx); err == nil {
		t.Fatal("CreateConversation must fail on a read-only store")
	}
	if _, err := s.FindOrCreateActor(ctx, s.Pool, "external", "x"); err != nil {
		t.Fatalf("the normal store still writes: %v", err)
	}
}
