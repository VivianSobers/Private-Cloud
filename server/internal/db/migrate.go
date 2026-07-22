package db

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Migrations are embedded in the binary rather than read from disk. The
// container therefore cannot drift from the code that expects the schema, and
// there is no "did you copy the migrations directory" failure mode.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// migrateMu serialises migrators within one process: goose's SetBaseFS,
// SetDialect and SetLogger are package globals, so two concurrent Migrate
// calls would race on them even against different databases.
var migrateMu sync.Mutex

// migrateLockKey identifies the cross-process advisory lock. Any constant
// works as long as every migrator of this schema uses the same one.
const migrateLockKey int64 = 0x70635f6d696772 // "pc_migr"

// Migrate applies all pending migrations.
//
// goose needs a database/sql handle, which pgx provides via its stdlib shim.
// We open a dedicated connection for migrations rather than borrowing from the
// pool: migrations can take locks for a long time, and they should not consume
// a connection that request handlers are waiting on.
//
// Concurrent migrators are safe, not just tolerated. Two processes racing
// goose on a fresh database both see "nothing applied" and both start issuing
// DDL — one loses with "relation already exists" halfway through, which is
// exactly what `go test ./...` does when two packages' fixtures migrate the
// same empty test database in parallel. A session-scoped Postgres advisory
// lock serialises them: the loser waits, then finds the schema already
// current and no-ops.
func (d *DB) Migrate(ctx context.Context, log *slog.Logger) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()

	// The advisory lock is session-scoped, so it must live on ONE pinned
	// connection for the whole migration — pool checkouts between statements
	// could unlock on a different session than the one that locked.
	lockConn, err := d.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer lockConn.Release()

	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("take migration advisory lock: %w", err)
	}
	defer func() {
		// WithoutCancel: the lock must be released even when the caller's
		// context died mid-migration, or every later migrator blocks forever.
		if _, err := lockConn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, migrateLockKey); err != nil {
			log.Warn("could not release migration advisory lock; it falls with the session", "error", err)
		}
	}()

	sqlDB := stdlib.OpenDBFromPool(d.Pool)
	defer sqlDB.Close()

	goose.SetBaseFS(migrationFS)
	goose.SetLogger(gooseLogger{log})

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	// Read under the lock: the version another migrator was mid-way through
	// applying is not a version, and reading it outside the lock is how two
	// processes both conclude the schema is empty.
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
