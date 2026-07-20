// Package db owns the Postgres connection pool and schema migrations.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool *pgxpool.Pool
	log  *slog.Logger
}

// Open builds the pool and blocks until Postgres answers, retrying with
// backoff. The retry matters: compose starts this container the moment
// Postgres reports healthy, but a pool built during Postgres's own startup
// window can still get connection refused. Crashing there would work — Docker
// restarts us — but it produces alarming logs for a routine race.
func Open(ctx context.Context, url string, maxConns, minConns int32, connectTimeout time.Duration, log *slog.Logger) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	cfg.MaxConns = maxConns
	cfg.MinConns = minConns
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	// Recycle connections so a long-lived pool doesn't pin server-side memory
	// or hold connections across a Postgres restart.
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	const maxAttempts = 10
	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		err := pool.Ping(pingCtx)
		cancel()
		if err == nil {
			log.Info("database connected", "attempt", attempt)
			return &DB{Pool: pool, log: log}, nil
		}
		lastErr = err

		// Respect shutdown while waiting; otherwise a failing DB makes the
		// process ignore SIGTERM for the whole retry budget.
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(backoff):
		}

		log.Warn("database not ready, retrying", "attempt", attempt, "error", err, "backoff", backoff)
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}

	pool.Close()
	return nil, fmt.Errorf("database unreachable after %d attempts: %w", maxAttempts, lastErr)
}

func (d *DB) Close() {
	d.Pool.Close()
}

// Ping is what /readyz calls. It takes a context so a hung database produces a
// failed readiness check rather than a hung readiness check.
func (d *DB) Ping(ctx context.Context) error {
	return d.Pool.Ping(ctx)
}
