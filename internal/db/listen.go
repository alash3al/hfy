package db

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Listen holds one dedicated connection (outside the query pool) for the
// process lifetime, LISTENs on the channels and calls fn for each
// notification. It reconnects with backoff; onReconnect runs after each
// re-LISTEN so callers can resync anything missed. It returns when ctx ends.
func (s *Store) Listen(ctx context.Context, fn func(channel, payload string), onReconnect func(), channels ...string) {
	backoff := time.Second
	first := true
	for ctx.Err() == nil {
		err := s.listenOnce(ctx, fn, func() {
			if !first && onReconnect != nil {
				onReconnect()
			}
			first = false
			backoff = time.Second
		}, channels)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("listen connection lost", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Store) listenOnce(ctx context.Context, fn func(string, string), ready func(), channels []string) error {
	conn, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	for _, ch := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			return err
		}
	}
	ready()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		fn(n.Channel, n.Payload)
	}
}
