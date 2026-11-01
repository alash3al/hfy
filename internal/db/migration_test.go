package db_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/db/dbtest"
)

// 00003 drops exactly the embeddings_<digits> tables of the current schema, the
// embed_events jobs that would never run and the backfill's index; it is safe
// to run again.
func TestDropEmbeddingsMigration(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Step back over 00004 and 00003 (its Down is a no-op), then leave a v1 database behind.
	down := func() {
		t.Helper()
		for range 2 {
			if _, err := s.Migrate(ctx, "down"); err != nil {
				t.Fatal(err)
			}
		}
	}
	down()
	gone := []string{"embeddings_3", "embeddings_1536", "embeddings_007"}
	kept := []string{"embeddings_x", "my_embeddings_3", "embeddings_", "embeddings_3x", "xembeddings_3", "Embeddings_3",
		"embeddings_1 ", "embeddings_3\n", `embeddings_1"; DROP TABLE jobs; --`}
	for _, n := range append(slices.Clone(gone), kept...) {
		exec(`CREATE TABLE ` + pgx.Identifier{n}.Sanitize() + ` (id int)`)
	}
	// Fails if a fresh install still creates it: the index is v1's, made here as v1 made it.
	exec(`CREATE INDEX conversation_events_message_text_idx ON conversation_events (id) WHERE type = 'message.text'`)
	exec(`CREATE INDEX conversation_events_other_idx ON conversation_events (id) WHERE type = 'message.text'`)
	exec(`CREATE VIEW embeddings_5 AS SELECT 1 AS id`)
	exec(`CREATE SCHEMA other`)
	exec(`CREATE TABLE other.embeddings_9 (id int)`)

	for _, j := range []struct{ typ, state string }{
		{"embed_events", "queued"}, {"embed_events", "running"}, {"embed_events", "failed"}, {"embed_events", "done"},
		{"run_turn", "queued"}, {"run_turn", "running"}, {"run_turn", "done"}, {"retention_cleanup", "queued"},
	} {
		exec(`INSERT INTO jobs (type, queue, state) VALUES ($1, 'q', $2)`, j.typ, j.state)
	}
	exec(`INSERT INTO jobs (type, queue, state) VALUES ('embed_events_extra', 'q', 'queued'), ('EMBED_EVENTS', 'q', 'queued')`)

	state := func() (tables, schemaOther, jobs []string) {
		t.Helper()
		col := func(q string) []string {
			rows, err := s.Pool.Query(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			out, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		tables = col(`SELECT tablename FROM pg_tables WHERE schemaname = current_schema() AND tablename ~* 'embeddings'`)
		slices.Sort(tables) // not ORDER BY: collations differ
		return tables,
			col(`SELECT tablename FROM pg_tables WHERE schemaname = 'other'`),
			col(`SELECT type || ':' || state FROM jobs ORDER BY id`)
	}
	wantTables := slices.Sorted(slices.Values(kept))
	wantJobs := []string{"embed_events:failed", "embed_events:done", "run_turn:queued", "run_turn:running", "run_turn:done",
		"retention_cleanup:queued", "embed_events_extra:queued", "EMBED_EVENTS:queued"}

	for run := 1; run <= 2; run++ { // the second run finds nothing left to do
		if _, err := s.Migrate(ctx, "up"); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		tables, other, jobs := state()
		if !slices.Equal(tables, wantTables) {
			t.Fatalf("run %d: tables %q, want %q", run, tables, wantTables)
		}
		if !slices.Equal(other, []string{"embeddings_9"}) {
			t.Fatalf("run %d: another schema's table was touched: %q", run, other)
		}
		if !slices.Equal(jobs, wantJobs) {
			t.Fatalf("run %d: jobs %q, want %q", run, jobs, wantJobs)
		}
		var view int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM embeddings_5`).Scan(&view); err != nil {
			t.Fatalf("run %d: the view must survive: %v", run, err)
		}
		var idx []string
		rows, err := s.Pool.Query(ctx, `SELECT indexname FROM pg_indexes WHERE schemaname = current_schema()
			AND indexname IN ('conversation_events_message_text_idx', 'conversation_events_other_idx') ORDER BY 1`)
		if err == nil {
			idx, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if err != nil || !slices.Equal(idx, []string{"conversation_events_other_idx"}) {
			t.Fatalf("run %d: indexes %q (%v), want only the decoy", run, idx, err)
		}
		if run == 1 {
			down() // so the same SQL runs again
		}
	}
}

// 00004 gives every grant that exists a chain of its own named by its uuid,
// not renewable, and leaves room for the previous release to keep inserting
// grants during a rolling deploy. Its Down drops what it added.
func TestGrantExchangeMigration(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := s.Migrate(ctx, "down"); err != nil { // back to before 00004
		t.Fatal(err)
	}
	exec(`INSERT INTO actors (type, ref) VALUES ('external', 'u1')`)
	exec(`INSERT INTO conversations SELECT FROM generate_series(1, 1)`)
	for i := range 3 { // grants as the previous release made them: none of the new columns
		exec(`INSERT INTO grants (token_hash, conversation_id, actor_id, scope, agent, expires_at)
			VALUES ($1, 1, 1, 'u1', '', now() + interval '1 hour')`, []byte{byte(i)})
	}
	if _, err := s.Migrate(ctx, "up"); err != nil {
		t.Fatal(err)
	}
	var same, total, renewable int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE root_id = uuid), count(*),
		count(renew_until) + count(renew_secs) FROM grants`).Scan(&same, &total, &renewable); err != nil {
		t.Fatal(err)
	}
	if same != 3 || total != 3 || renewable != 0 {
		t.Fatalf("backfill: root_id = uuid on %d of %d rows, %d renewable values", same, total, renewable)
	}
	// the previous release inserts without root_id: it must still work, as a chain of one
	exec(`INSERT INTO grants (token_hash, conversation_id, actor_id, scope, agent, expires_at)
		VALUES ('\x09', 1, 1, 'u1', '', now() + interval '1 hour')`)
	var nullable string
	if err := s.Pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'grants' AND column_name = 'root_id'`).Scan(&nullable); err != nil || nullable != "NO" {
		t.Fatalf("root_id nullable %q: %v", nullable, err)
	}
	var indexed bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = current_schema()
		AND tablename = 'grants' AND indexdef LIKE '%(root_id)')`).Scan(&indexed); err != nil || !indexed {
		t.Fatalf("no index on root_id: %v", err)
	}

	if _, err := s.Migrate(ctx, "down"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema()
		AND table_name = 'grants' AND column_name IN ('renew_until', 'renew_secs', 'root_id')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("down left %d columns: %v", left, err)
	}
	if _, err := s.Migrate(ctx, "up"); err != nil { // and up works over rows that exist
		t.Fatal(err)
	}
}
