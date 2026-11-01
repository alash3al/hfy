package db_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/db/dbtest"
)

// 00003 drops exactly the embeddings_<digits> tables of the current schema and
// the embed_events jobs that would never run; it is safe to run again.
func TestDropEmbeddingsMigration(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Step back over 00003 (its Down is a no-op), then leave a v1 database behind.
	if _, err := s.Migrate(ctx, "down"); err != nil {
		t.Fatal(err)
	}
	gone := []string{"embeddings_3", "embeddings_1536", "embeddings_007"}
	kept := []string{"embeddings_x", "my_embeddings_3", "embeddings_", "embeddings_3x", "xembeddings_3", "Embeddings_3",
		"embeddings_1 ", "embeddings_3\n", `embeddings_1"; DROP TABLE jobs; --`}
	for _, n := range append(slices.Clone(gone), kept...) {
		exec(`CREATE TABLE ` + pgx.Identifier{n}.Sanitize() + ` (id int)`)
	}
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
		if run == 1 {
			if _, err := s.Migrate(ctx, "down"); err != nil { // so the same SQL runs again
				t.Fatal(err)
			}
		}
	}
}
