package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/db/dbtest"
)

// TestHotQueriesUseIndexes seeds a realistic amount of data and checks the hot
// queries do not fall back to sequential scans of the big tables.
func TestHotQueriesUseIndexes(t *testing.T) {
	s := dbtest.New(t)
	ctx := context.Background()
	mustExec := func(sql string) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO actors (type, ref) VALUES ('external', 'u1')`)
	mustExec(`INSERT INTO conversations SELECT FROM generate_series(1, 5)`)
	// 5 conversations x 600 turns, each with a message, a state, usage and 4 deltas
	mustExec(`INSERT INTO conversation_events (conversation_id, actor_id, scope, type, data)
		SELECT s.id, 1, 'u1', 'turn', '{}' FROM conversations s, generate_series(1, 600)`)
	mustExec(`INSERT INTO conversation_events (conversation_id, parent_id, actor_id, scope, type, data, ephemeral)
		SELECT t.conversation_id, t.id, 1, 'u1', k.type, '{"text":"x"}', k.eph
		FROM conversation_events t,
		     (VALUES ('message.text', false), ('turn.state', false), ('model.usage', false),
		             ('text.delta', true), ('text.delta', true), ('text.delta', true), ('text.delta', true)) AS k(type, eph)
		WHERE t.type = 'turn'`)
	mustExec(`INSERT INTO jobs (type, queue, payload, state, concurrency_key)
		SELECT 'run_turn', 'turns', '{}', CASE WHEN g % 50 = 0 THEN 'queued' ELSE 'done' END, 'conversation:' || (g % 5)
		FROM generate_series(1, 20000) g`)
	mustExec(`ANALYZE`)

	var mid, last int64
	s.Pool.QueryRow(ctx, `SELECT id FROM conversation_events WHERE conversation_id = 1 AND type = 'turn' ORDER BY id OFFSET 300 LIMIT 1`).Scan(&mid)
	s.Pool.QueryRow(ctx, `SELECT max(id) FROM conversation_events WHERE conversation_id = 1 AND type = 'turn'`).Scan(&last)

	cases := []struct {
		name string
		args pgx.NamedArgs
		bad  []string
	}{
		{"LoadContext", pgx.NamedArgs{"conversation": 1, "after_turn": mid, "upto_turn": last}, []string{"Seq Scan on conversation_events"}},
		{"EventsAfter", pgx.NamedArgs{"conversation": 1, "after": mid, "limit": 500}, []string{"Seq Scan on conversation_events"}},
		{"HistoryTurns", pgx.NamedArgs{"conversation": 1, "before": last, "limit": 20}, []string{"Seq Scan on conversation_events"}},
		{"HistoryEvents", pgx.NamedArgs{"conversation": 1, "turns": []int64{mid, mid + 1, mid + 2}}, []string{"Seq Scan on conversation_events"}},
		{"TurnEvents", pgx.NamedArgs{"turn": mid}, []string{"Seq Scan on conversation_events"}},
		{"EventIDByUUID", pgx.NamedArgs{"uuid": "00000000-0000-4000-8000-000000000000", "conversation": 1}, []string{"Seq Scan on conversation_events"}},
		{"LatestEventID", pgx.NamedArgs{"conversation": 1}, []string{"Seq Scan on conversation_events"}},
		{"ClaimJob", pgx.NamedArgs{"queue": "turns", "lease": "30s"}, []string{"Seq Scan on jobs"}},
		{"ReapJobs", pgx.NamedArgs{}, []string{"Seq Scan on jobs"}},
		{"DeleteExpiredEvents", pgx.NamedArgs{"age": "720h", "limit": 5000}, []string{"Seq Scan on conversation_events"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) // EXPLAIN (no ANALYZE) does not run it, but be safe
			rows, err := tx.Query(ctx, "EXPLAIN "+s.SQL(c.name), c.args)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Join(plan, "\n")
			for _, bad := range c.bad {
				if strings.Contains(text, bad) {
					t.Errorf("%s uses %q:\n%s", c.name, bad, text)
				}
			}
		})
	}
}
