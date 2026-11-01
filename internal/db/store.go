// Package db is all Postgres access: pool, embedded migrations and queries,
// and one named function per use case. There is no ORM.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed queries/*.sql
var queriesFS embed.FS

// Postgres notification channels.
const (
	ChanEvents = "hfy_events" // payload "<conversation id>:<event id>"
	ChanCancel = "hfy_cancel" // payload "<turn event id>"
	ChanJobs   = "hfy_jobs"   // payload "<queue>"
)

// DBTX is satisfied by *pgxpool.Pool and pgx.Tx.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	Pool *pgxpool.Pool

	sql map[string]string
}

// queryNames lists every query file the code uses; a missing file is a startup error.
var queryNames = []string{
	"InsertActor", "SelectActor", "InsertConversation", "SelectConversationByUUID", "LockConversation",
	"SetActiveTurn", "SetSummary", "AddConversationUsage", "InsertEvent", "SelectEventByID",
	"EventIDByUUID", "EventsAfter", "LatestEventID", "HistoryTurns", "HistoryEvents", "TurnEvents",
	"LoadContext", "LatestTurnState", "QueuedTurns", "TurnUsage", "SelectSummary", "TurnIDByUUID",
	"EnqueueJob", "ClaimJob", "HeartbeatJob", "CompleteJob", "RetryJob", "ReleaseJob", "ReapJobs", "JobCounts",
	"CancelQueuedTurnJobs", "PendingTurnJob", "SelectConversationByID", "OpenTurns", "SettleTurns", "PruneJobs", "DeleteExpiredEvents",
	"DeleteExpiredOutbound", "DeleteExpiredGrants", "InsertOutbound",
	"InsertGrant", "SelectGrantByToken", "GrantLive", "RevokeGrant", "ExchangeGrant", "ExchangeMiss", "ResolveGrantChain", "LockGrantChain", "LockGrantChainOfToken",
	"InsertConversationFile", "LatestTurn",
}

// Open connects, pings and loads the embedded queries.
func Open(ctx context.Context, url string, maxConns int32) (*Store, error) {
	return open(ctx, url, maxConns, false)
}

// OpenReadOnly is Open for commands that only look: every session starts
// read-only, so a write fails in Postgres instead of happening.
func OpenReadOnly(ctx context.Context, url string, maxConns int32) (*Store, error) {
	return open(ctx, url, maxConns, true)
}

func open(ctx context.Context, url string, maxConns int32, readOnly bool) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("storage.url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	// hfy's queries are short and indexed. Where a bound is known only at run
	// time (settled_until), the planner estimates a full scan and Postgres
	// would JIT-compile a query that then reads a few pages: milliseconds of
	// compiling for microseconds of work. A URL's own jit setting wins.
	if _, set := cfg.ConnConfig.RuntimeParams["jit"]; !set {
		cfg.ConnConfig.RuntimeParams["jit"] = "off"
	}
	if readOnly {
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres unreachable: %w", err)
	}
	s := &Store{Pool: pool}
	if s.sql, err = loadQueries(); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.Pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

func loadQueries() (map[string]string, error) {
	out := map[string]string{}
	entries, err := fs.ReadDir(queriesFS, "queries")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := queriesFS.ReadFile("queries/" + e.Name())
		if err != nil {
			return nil, err
		}
		out[strings.TrimSuffix(e.Name(), ".sql")] = strings.TrimSpace(string(b))
	}
	for _, n := range queryNames {
		if out[n] == "" {
			return nil, fmt.Errorf("embedded query %s.sql is missing or empty", n)
		}
	}
	return out, nil
}

// q returns a query by name.
func (s *Store) q(name string) string {
	text, ok := s.sql[name]
	if !ok {
		panic("db: unknown query " + name)
	}
	return text
}

// Tx runs fn in a transaction.
func (s *Store) Tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, fn)
}

func collect[T any](rows pgx.Rows, err error) ([]T, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[T])
}

func one[T any](rows pgx.Rows, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}
	return pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[T])
}

// IsNotFound reports pgx.ErrNoRows.
func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// IsUniqueViolation reports a Postgres unique-constraint error.
func IsUniqueViolation(err error) bool {
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pe.Code == "23505"
	}
	return false
}

// Migrate runs goose: "up", "down" or "status". Concurrent processes are
// serialized by a Postgres advisory lock.
func (s *Store) Migrate(ctx context.Context, cmd string) (string, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return "", err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return "", err
	}
	sqlDB := stdlib.OpenDBFromPool(s.Pool)
	defer sqlDB.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	switch cmd {
	case "up":
		res, err := p.Up(ctx)
		for _, r := range res {
			fmt.Fprintf(&out, "applied %s\n", r.Source.Path)
		}
		if err == nil {
			s.warnIfSchemaIsNewer(ctx, p)
		}
		return out.String(), err
	case "down":
		res, err := p.Down(ctx)
		if res != nil {
			fmt.Fprintf(&out, "reverted %s\n", res.Source.Path)
		}
		return out.String(), err
	case "status":
		sts, err := p.Status(ctx)
		for _, st := range sts {
			fmt.Fprintf(&out, "%-30s %s\n", st.Source.Path, st.State)
		}
		return out.String(), err
	}
	return "", fmt.Errorf("unknown migrate command %q (up|down|status)", cmd)
}

// warnIfSchemaIsNewer flags a binary running against a schema migrated by a
// newer hfy. That is allowed (rolling deploys): migrations must stay
// compatible with the previous release, adding before removing.
func (s *Store) warnIfSchemaIsNewer(ctx context.Context, p *goose.Provider) {
	dbv, err := p.GetDBVersion(ctx)
	if err != nil {
		return
	}
	srcs := p.ListSources()
	if known := srcs[len(srcs)-1].Version; dbv > known {
		slog.Warn("database schema is newer than this hfy; running in compatibility mode",
			"schema", dbv, "known", known)
	}
}
