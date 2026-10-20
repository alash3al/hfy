// Package dbtest gives tests an isolated, migrated database on a real
// Postgres with pgvector (HFY_TEST_DB, default the local test container).
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/db"
)

const defaultAdmin = "postgres://postgres:hfy@127.0.0.1:54330/hfy?sslmode=disable"

// New creates a fresh database, migrates it and returns an open store.
// The test is skipped when no Postgres is reachable.
func New(t testing.TB, emb *db.Embeddings) *db.Store {
	t.Helper()
	admin := os.Getenv("HFY_TEST_DB")
	if admin == "" {
		admin = defaultAdmin
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Skipf("no test postgres: %v", err)
	}
	defer conn.Close(ctx)

	b := make([]byte, 6)
	rand.Read(b)
	name := "hfy_t_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	url := strings.Replace(admin, "/hfy?", "/"+name+"?", 1)
	if !strings.Contains(url, name) { // admin url without query string
		url = strings.TrimSuffix(admin, "/hfy") + "/" + name
	}
	s, err := db.Open(ctx, url, 20, emb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx, "up"); err != nil {
		t.Fatal(err)
	}
	if err := s.Boot(ctx, 3); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		c, err := pgx.Connect(ctx, admin)
		if err == nil {
			c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(ctx)
		}
	})
	return s
}
