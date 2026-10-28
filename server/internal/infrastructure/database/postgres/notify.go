package postgres

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Listen LISTENs on channel and signals wake for every notification until
// ctx ends. It uses one dedicated connection outside the pool, so the
// LISTEN never lingers on a pooled connection other queries reuse, and it
// closes that connection when it returns.
//
// A signal never blocks: wake has room for one, and a notification that
// arrives while one is pending changes nothing, because the receiver takes
// everything that is due. A lost connection is retried after a growing
// pause; the caller's own polling carries on meanwhile.
func (db *DB) Listen(ctx context.Context, channel string, wake chan<- struct{}, log *slog.Logger) {
	pause := time.Second
	for ctx.Err() == nil {
		connected, err := db.listenOnce(ctx, channel, wake)
		if ctx.Err() != nil {
			return
		}
		if connected {
			pause = time.Second
		}
		log.Warn("database listener lost; polling until it reconnects", "channel", channel, "error", err, "retry_in", pause)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		pause = min(pause*2, 30*time.Second)
	}
}

func (db *DB) listenOnce(ctx context.Context, channel string, wake chan<- struct{}) (bool, error) {
	conn, err := pgx.ConnectConfig(ctx, db.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return false, err
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closing)
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return false, err
	}
	// Anything committed while nothing was listening is picked up now.
	signal(wake)
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return true, err
		}
		signal(wake)
	}
}

func signal(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}
