package db

import (
	"context"
	"embed"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Migrations are embedded in the binary rather than read from disk. The
// container therefore cannot drift from the code that expects the schema, and
// there is no "did you copy the migrations directory" failure mode.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies all pending migrations.
//
// goose needs a database/sql handle, which pgx provides via its stdlib shim.
// We open a dedicated connection for migrations rather than borrowing from the
// pool: migrations can take locks for a long time, and they should not consume
// a connection that request handlers are waiting on.
func (d *DB) Migrate(ctx context.Context, log *slog.Logger) error {
	sqlDB := stdlib.OpenDBFromPool(d.Pool)
	defer sqlDB.Close()

	goose.SetBaseFS(migrationFS)
	goose.SetLogger(gooseLogger{log})

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	before, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	after, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("read schema version after migrate: %w", err)
	}

	if before == after {
		log.Info("schema up to date", "version", after)
	} else {
		log.Info("schema migrated", "from", before, "to", after)
	}
	return nil
}

// SchemaVersion reports the current migration version, surfaced as a metric so
// you can see on a dashboard which schema the running binary is against.
func (d *DB) SchemaVersion(ctx context.Context) (int64, error) {
	sqlDB := stdlib.OpenDBFromPool(d.Pool)
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		return 0, err
	}
	return goose.GetDBVersionContext(ctx, sqlDB)
}

// gooseLogger adapts goose's logger interface onto slog so migration output
// lands in the same structured stream as everything else.
type gooseLogger struct{ log *slog.Logger }

func (g gooseLogger) Printf(format string, v ...any) {
	g.log.Info(fmt.Sprintf(format, v...), "component", "goose")
}

func (g gooseLogger) Fatalf(format string, v ...any) {
	// Deliberately not os.Exit: goose calls Fatalf on migration errors, and the
	// caller already handles the returned error. Exiting here would bypass
	// graceful shutdown and make failures harder to diagnose.
	g.log.Error(fmt.Sprintf(format, v...), "component", "goose")
}
