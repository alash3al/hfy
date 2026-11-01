package db_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
)

// stepDown steps the schema back n migrations.
func stepDown(t *testing.T, s *db.Store, n int) {
	t.Helper()
	for range n {
		if _, err := s.Migrate(context.Background(), "down"); err != nil {
			t.Fatal(err)
		}
	}
}

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
	// Step back over 00005, 00004 and 00003 (its Down is a no-op), then leave a v1 database behind.
	down := func() {
		t.Helper()
		for range 3 {
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

// 00004 only adds nullable columns, so it is metadata-only: no table rewrite.
// Grants that exist, or that the previous release keeps inserting during a rolling
// deploy, have no root_id and are chains of one named by their uuid; new grants
// and their exchanges carry one. Its Down ends the later rows of every chain,
// which the previous release could not revoke, and drops what it added.
func TestGrantExchangeMigration(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	stepDown(t, s, 2) // back to before 00004
	exec(`INSERT INTO actors (type, ref) VALUES ('external', 'u1')`)
	exec(`INSERT INTO conversations SELECT FROM generate_series(1, 1)`)
	oldGrant := func(hash byte) (id uuid.UUID) { // as the previous release made it: none of the new columns
		t.Helper()
		if err := s.Pool.QueryRow(ctx, `INSERT INTO grants (token_hash, conversation_id, actor_id, scope, agent, expires_at)
			VALUES ($1, 1, 1, 'u1', '', now() + interval '1 hour') RETURNING uuid`, []byte{hash}).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	before := oldGrant(1)

	// the table keeps its rows as they are: a rewrite would change the relation's file
	var fileBefore int64
	s.Pool.QueryRow(ctx, `SELECT relfilenode FROM pg_class WHERE oid = 'grants'::regclass`).Scan(&fileBefore)
	if _, err := s.Migrate(ctx, "up"); err != nil {
		t.Fatal(err)
	}
	var fileAfter int64
	s.Pool.QueryRow(ctx, `SELECT relfilenode FROM pg_class WHERE oid = 'grants'::regclass`).Scan(&fileAfter)
	if fileBefore != fileAfter {
		t.Fatal("00004 rewrote the grants table")
	}
	var nulls, extras int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE root_id IS NULL), count(renew_until) + count(renew_secs) FROM grants`).Scan(&nulls, &extras); err != nil {
		t.Fatal(err)
	}
	if nulls != 1 || extras != 0 {
		t.Fatalf("existing grants: %d without root_id of 1, %d renewable values", nulls, extras)
	}
	var defaults int
	s.Pool.QueryRow(ctx, `SELECT count(column_default) FROM information_schema.columns WHERE table_schema = current_schema()
		AND table_name = 'grants' AND column_name IN ('renew_until', 'renew_secs', 'root_id')`).Scan(&defaults)
	if defaults != 0 {
		t.Fatalf("%d of the new columns have a default: adding them would rewrite the table", defaults)
	}

	// the previous release still inserts; its grants revoke by their own uuid, and only theirs
	late := oldGrant(2)
	other := oldGrant(3)
	if err := s.RevokeGrant(ctx, late); err != nil {
		t.Fatalf("revoke a grant without root_id: %v", err)
	}
	rows, err := s.Pool.Query(ctx, `SELECT uuid FROM grants WHERE expires_at > now() AND root_id IS NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	liveOld, _ := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if !slices.Equal(liveOld, []uuid.UUID{before, other}) {
		t.Fatalf("live old grants %v, want %v and %v", liveOld, before, other)
	}
	if err := s.RevokeGrant(ctx, uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown id: %v", err)
	}

	// a new grant and its exchanges: one chain, revoked by the first id
	id, _, err := s.CreateGrant(ctx, db.NewGrant{TokenHash: []byte("c0"), ConversationID: 1, ActorID: 1, Scope: "u1",
		ExpiresIn: time.Minute, MaxLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"c1", "c2"} {
		prev := "c" + string(rune(h[1]-1))
		if _, err := s.ExchangeGrant(ctx, []byte(prev), []byte(h), time.Minute, 0); err != nil {
			t.Fatal(err)
		}
	}
	var roots int
	s.Pool.QueryRow(ctx, `SELECT count(*) FROM grants WHERE root_id = $1`, id).Scan(&roots)
	if roots != 3 {
		t.Fatalf("%d rows in the chain", roots)
	}

	// Down: the chain's later rows end (the first stays, no longer renewable), the
	// grants of the previous release are untouched
	stepDown(t, s, 2)
	var left int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema()
		AND table_name = 'grants' AND column_name IN ('renew_until', 'renew_secs', 'root_id')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("down left %d columns: %v", left, err)
	}
	rows, err = s.Pool.Query(ctx, `SELECT encode(token_hash, 'hex') FROM grants WHERE expires_at > now() ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	live, err := pgx.CollectRows(rows, pgx.RowTo[string])
	// c0, the first row, was only retired into its grace; c1 and c2 are the later rows
	if err != nil || !slices.Equal(live, []string{"01", "03", "6330"}) {
		t.Fatalf("live after down: %q (%v)", live, err)
	}
	if _, err := s.Migrate(ctx, "up"); err != nil { // and up works over rows that exist
		t.Fatal(err)
	}
}

// 00004 gives up when a long transaction holds the grants table, instead of
// queueing its ALTER behind it and blocking every token lookup that follows.
func TestGrantExchangeMigrationDoesNotWaitForeverForTheTable(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	stepDown(t, s, 2) // back to before 00004

	long, err := s.Pool.Begin(ctx) // a long transaction that has read grants
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			long.Rollback(ctx)
		}
	}
	defer release()
	if _, err := long.Exec(ctx, `SELECT count(*) FROM grants`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Migrate(ctx, "up"); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lock timeout") {
			t.Fatalf("want a lock timeout, got %v", err)
		}
	case <-time.After(15 * time.Second):
		release() // let the stuck migration finish before the database goes
		<-done
		t.Fatal("the migration waited for the table without a timeout")
	}
	release()
	if _, err := s.Migrate(ctx, "up"); err != nil { // nothing is left half done
		t.Fatalf("running it again: %v", err)
	}
}

// 00005 adds the nullable key of an event and the partial unique index over
// it. Events that exist, and those the previous release keeps inserting, have no
// key and are not in the index; a key is unique per (conversation, actor), and
// case-sensitive. Down takes both away and keeps the events. It is safe to run
// again, also after a build that was cancelled and left an invalid index.
func TestEventKeyMigration(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	exec := func(q string, args ...any) error {
		t.Helper()
		_, err := s.Pool.Exec(ctx, q, args...)
		return err
	}
	must := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	stepDown(t, s, 1) // back to before 00005
	must(`INSERT INTO actors (type, ref) VALUES ('host', 'crm'), ('host', 'erp')`)
	must(`INSERT INTO conversations SELECT FROM generate_series(1, 2)`)
	insert := func(conv, actor int, key any) error {
		return exec(`INSERT INTO conversation_events (conversation_id, actor_id, scope, type, key) VALUES ($1, $2, 's', 'context.note', $3)`, conv, actor, key)
	}
	must(`INSERT INTO conversation_events (conversation_id, actor_id, scope, type) SELECT 1, 1, 's', 'turn' FROM generate_series(1, 50)`)
	var fileBefore int64
	s.Pool.QueryRow(ctx, `SELECT relfilenode FROM pg_class WHERE oid = 'conversation_events'::regclass`).Scan(&fileBefore)

	for run := 1; run <= 2; run++ { // twice: the migration finds its own work done
		if run == 2 {
			must(`DELETE FROM goose_db_version WHERE version_id = 5`)
		}
		if _, err := s.Migrate(ctx, "up"); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		var fileAfter int64
		s.Pool.QueryRow(ctx, `SELECT relfilenode FROM pg_class WHERE oid = 'conversation_events'::regclass`).Scan(&fileAfter)
		var rows, keyed int
		var valid bool
		var def, deflt string
		s.Pool.QueryRow(ctx, `SELECT count(*), count(key) FROM conversation_events`).Scan(&rows, &keyed)
		if err := s.Pool.QueryRow(ctx, `SELECT i.indisvalid, pg_get_indexdef(i.indexrelid) FROM pg_index i WHERE i.indexrelid = 'conversation_events_key_idx'::regclass`).Scan(&valid, &def); err != nil {
			t.Fatalf("run %d: index: %v", run, err)
		}
		s.Pool.QueryRow(ctx, `SELECT coalesce(column_default, '') FROM information_schema.columns WHERE table_name = 'conversation_events' AND column_name = 'key'`).Scan(&deflt)
		if fileBefore != fileAfter || rows != 50 || keyed != 0 || deflt != "" {
			t.Fatalf("run %d: rewritten=%v rows=%d keyed=%d default=%q", run, fileBefore != fileAfter, rows, keyed, deflt)
		}
		if !valid || !strings.Contains(def, "UNIQUE") || !strings.Contains(def, "(conversation_id, actor_id, key)") || !strings.Contains(def, "WHERE (key IS NOT NULL)") {
			t.Fatalf("run %d: index %v %s", run, valid, def)
		}
		if run == 1 { // a build that was cancelled leaves an invalid index of the same name: the next run replaces it
			must(`UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'conversation_events_key_idx'::regclass`)
		}
	}

	// semantics: events without a key never collide; a key does, per conversation and actor
	for range 3 {
		if err := insert(1, 1, nil); err != nil {
			t.Fatalf("events without a key must repeat: %v", err)
		}
	}
	if err := insert(1, 1, "k"); err != nil {
		t.Fatal(err)
	}
	if err := insert(1, 1, "k"); !db.IsUniqueViolation(err) {
		t.Fatalf("the same key again: %v", err)
	}
	for name, c := range map[string]struct {
		conv, actor int
		key         string
	}{"another actor": {1, 2, "k"}, "another conversation": {2, 1, "k"}, "case": {1, 1, "K"}, "another key": {1, 1, "k2"}, "empty": {1, 1, ""}} {
		if err := insert(c.conv, c.actor, c.key); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := insert(1, 1, ""); !db.IsUniqueViolation(err) {
		t.Errorf("the empty key is a key: %v", err)
	}

	// the previous release inserts without naming key: it still works
	must(`INSERT INTO conversation_events (conversation_id, actor_id, scope, type, data, v, ephemeral, request_id)
		VALUES (1, 1, 's', 'turn', '{}', 1, false, NULL)`)

	stepDown(t, s, 1)
	var cols, idx, rows int
	s.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'conversation_events' AND column_name = 'key'`).Scan(&cols)
	s.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 'conversation_events_key_idx'`).Scan(&idx)
	s.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_events`).Scan(&rows)
	if cols != 0 || idx != 0 || rows != 50+3+1+5+1 {
		t.Fatalf("down left %d columns and %d indexes, %d rows", cols, idx, rows)
	}
	if _, err := s.Migrate(ctx, "up"); err != nil { // and up works over events that exist
		t.Fatal(err)
	}
}

// 00005 gives up when a long transaction holds the events table, instead of
// queueing its ALTER behind it and blocking every insert that follows.
func TestEventKeyMigrationDoesNotWaitForeverForTheTable(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	stepDown(t, s, 1)
	long, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			long.Rollback(ctx)
		}
	}
	defer release()
	if _, err := long.Exec(ctx, `SELECT count(*) FROM conversation_events`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Migrate(ctx, "up"); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lock timeout") {
			t.Fatalf("want a lock timeout, got %v", err)
		}
	case <-time.After(15 * time.Second):
		release()
		<-done
		t.Fatal("the migration waited for the table without a timeout")
	}
	release()
	if _, err := s.Migrate(ctx, "up"); err != nil { // nothing is left half done
		t.Fatalf("running it again: %v", err)
	}
}

// The index is built CONCURRENTLY, outside a transaction: a plain CREATE INDEX
// holds a lock that stops every insert for as long as it reads the table.
func TestEventKeyIndexIsBuiltConcurrently(t *testing.T) {
	b, err := os.ReadFile("migrations/00005_event_key.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(b), "-- +goose Down")
	for _, want := range []string{"-- +goose NO TRANSACTION", "CREATE UNIQUE INDEX CONCURRENTLY", "SET LOCAL lock_timeout"} {
		if !strings.Contains(up, want) {
			t.Errorf("00005 lacks %q", want)
		}
	}
}
