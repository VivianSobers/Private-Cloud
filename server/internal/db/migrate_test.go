package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Migrations are the one part of the system with no undo. A down migration that
// does not parse is discovered at the worst possible moment — mid-incident,
// trying to roll back — so these tests apply every migration forwards, then
// backwards to nothing, then forwards again.
//
// Each test gets its OWN database, created and dropped here. Running `DownTo 0`
// against the shared integration database would delete the schema out from
// under every other test in the suite.
//
// Run with:
//
//	PC_TEST_DATABASE_URL=postgres://... go test ./internal/db/...

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// scratchDB creates a throwaway database and returns a pool connected to it.
func scratchDB(t *testing.T) *DB {
	t.Helper()

	adminDSN := os.Getenv("PC_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PC_TEST_DATABASE_URL not set; skipping migration tests")
	}

	log := quietLog()
	ctx := context.Background()

	admin, err := Open(ctx, adminDSN, 2, 1, 10*time.Second, log)
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}
	defer admin.Close()

	// Random rather than derived from the test name: two runs of the same test
	// against the same server must not collide, and a leaked database from a
	// killed run must not block the next one.
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random: %v", err)
	}
	name := "pc_migtest_" + hex.EncodeToString(suffix[:])

	// CREATE DATABASE cannot run inside a transaction, so this goes straight at
	// the connection. The identifier is generated here and contains only
	// [a-z0-9_], so there is nothing to inject.
	if _, err := admin.Pool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch database %s: %v", name, err)
	}

	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse PC_TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name

	scratch, err := Open(ctx, u.String(), 4, 1, 10*time.Second, log)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}

	t.Cleanup(func() {
		scratch.Close()

		// A fresh admin pool: the deferred Close above has already run by now.
		cleanupAdmin, err := Open(context.Background(), adminDSN, 2, 1, 10*time.Second, log)
		if err != nil {
			t.Logf("could not reopen admin database to drop %s: %v", name, err)
			return
		}
		defer cleanupAdmin.Close()

		// WITH (FORCE) terminates leftover backends. Without it a single
		// connection the pool has not finished closing makes the DROP hang
		// until the test binary exits, leaking a database per run.
		if _, err := cleanupAdmin.Pool.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("could not drop scratch database %s: %v", name, err)
		}
	})

	return scratch
}

// gooseHandle wires goose up against the scratch pool the same way Migrate does.
func gooseHandle(t *testing.T, d *DB) *sql.DB {
	t.Helper()
	goose.SetBaseFS(migrationFS)
	goose.SetLogger(gooseLogger{quietLog()})
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	sqlDB := stdlib.OpenDBFromPool(d.Pool)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// latestVersion reads the highest migration number out of the embedded FS, so
// adding migration 8 does not require editing this test.
func latestVersion(t *testing.T) int64 {
	t.Helper()
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}

	var highest int64
	var count int
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		count++
		numeric, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			t.Fatalf("migration %q does not follow NNNNN_name.sql", e.Name())
		}
		v, err := strconv.ParseInt(numeric, 10, 64)
		if err != nil {
			t.Fatalf("migration %q has a non-numeric version: %v", e.Name(), err)
		}
		if v > highest {
			highest = v
		}
	}

	if count == 0 {
		// The go:embed directive silently produces an empty FS if the pattern
		// stops matching, and every migration would then be a no-op.
		t.Fatal("no migrations found in the embedded FS")
	}
	return highest
}

func TestMigrationsApplyToLatest(t *testing.T) {
	d := scratchDB(t)
	ctx := context.Background()

	if err := d.Migrate(ctx, quietLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := d.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if want := latestVersion(t); got != want {
		t.Fatalf("schema version = %d, want %d", got, want)
	}

	// Migrate is called on every boot, so it has to be a no-op the second time.
	if err := d.Migrate(ctx, quietLog()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// The round trip is what proves the down migrations are real. A down that drops
// a table but forgets its trigger function, or leaves a constraint behind,
// fails on the way back up rather than silently at 3am.
func TestMigrationsRoundTrip(t *testing.T) {
	d := scratchDB(t)
	ctx := context.Background()
	sqlDB := gooseHandle(t, d)
	latest := latestVersion(t)

	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		t.Fatalf("initial up: %v", err)
	}
	if !tableExists(t, d, "chunks") {
		t.Fatal("chunks table missing after up")
	}

	if err := goose.DownToContext(ctx, sqlDB, "migrations", 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	v, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		t.Fatalf("version after down: %v", err)
	}
	if v != 0 {
		t.Fatalf("version after down = %d, want 0", v)
	}
	// Every table the migrations created must be gone. A down migration that
	// quietly leaves one behind makes the next up fail with "already exists".
	for _, tbl := range []string{
		"users", "webauthn_credentials", "recovery_codes", "sessions",
		"webauthn_ceremonies", "blobs", "nodes", "file_versions",
		"upload_sessions", "app_passwords",
		"chunks", "manifests", "manifest_chunks",
		"shares", "sync_state", "changes", "jobs", "doc_text", "doc_embedding", "node_tags", "oidc_identities",
	} {
		if tableExists(t, d, tbl) {
			t.Errorf("table %q survived the down migration", tbl)
		}
	}

	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	v, err = goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		t.Fatalf("version after second up: %v", err)
	}
	if v != latest {
		t.Fatalf("version after second up = %d, want %d", v, latest)
	}
	if !tableExists(t, d, "chunks") {
		t.Fatal("chunks table missing after the second up")
	}
}

// Migration 7 down destroys the only record of how a manifest-backed file is
// assembled. It must refuse rather than orphan the content — and refuse
// without having already dropped anything.
func TestMigration00007DownRefusesToOrphanManifests(t *testing.T) {
	d := scratchDB(t)
	ctx := context.Background()
	sqlDB := gooseHandle(t, d)

	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		t.Fatalf("up: %v", err)
	}

	seedManifestBackedVersion(t, d)

	err := goose.DownToContext(ctx, sqlDB, "migrations", 6)
	if err == nil {
		t.Fatal("down migration succeeded with a manifest-backed version present; " +
			"that would have orphaned the file's contents")
	}
	if !strings.Contains(err.Error(), "cannot roll back") {
		t.Fatalf("down failed for the wrong reason: %v", err)
	}

	// The refusal must leave the schema exactly as it was. goose runs the
	// migration in a transaction, but the check is deliberately the first
	// statement so the safety does not depend on that.
	v, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		t.Fatalf("version after refused down: %v", err)
	}
	if v != 7 {
		t.Fatalf("version after refused down = %d, want 7", v)
	}
	if !columnExists(t, d, "file_versions", "manifest_id") {
		t.Fatal("file_versions.manifest_id was dropped despite the refusal")
	}
	if !tableExists(t, d, "manifest_chunks") {
		t.Fatal("manifest_chunks was dropped despite the refusal")
	}

	var n int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM file_versions WHERE manifest_id IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count manifest-backed versions: %v", err)
	}
	if n != 1 {
		t.Fatalf("manifest-backed versions = %d, want 1", n)
	}
}

// The refusal must not fire when every version is still a whole-file blob —
// otherwise Phase 1 deployments could never roll the migration back at all.
func TestMigration00007DownAllowsBlobOnlyVersions(t *testing.T) {
	d := scratchDB(t)
	ctx := context.Background()
	sqlDB := gooseHandle(t, d)

	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		t.Fatalf("up: %v", err)
	}

	seedBlobBackedVersion(t, d)

	if err := goose.DownToContext(ctx, sqlDB, "migrations", 6); err != nil {
		t.Fatalf("down to 6 with only blob-backed versions: %v", err)
	}
	if columnExists(t, d, "file_versions", "manifest_id") {
		t.Fatal("manifest_id column survived the down migration")
	}

	// And the Phase 1 row is intact, still readable by the old schema.
	var n int64
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM file_versions`).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if n != 1 {
		t.Fatalf("file_versions rows = %d, want 1", n)
	}
}

// The refcount trigger is the piece with the worst failure mode: an undercount
// deletes a chunk that another user's file still points at.
func TestChunkRefcountTrigger(t *testing.T) {
	d := scratchDB(t)
	ctx := context.Background()

	if err := d.Migrate(ctx, quietLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	hash := make([]byte, 32)
	if _, err := rand.Read(hash); err != nil {
		t.Fatalf("random: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO chunks (hash, size, stored_size, compression, storage_key)
		 VALUES ($1, 10, 10, 'none', $2)`, hash, hex.EncodeToString(hash)); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	// Two manifests referencing the same chunk, and one of them referencing it
	// twice — a file of repeated content. Refcount must be 3, not 2.
	var m1, m2 uuid.UUID
	for _, m := range []*uuid.UUID{&m1, &m2} {
		if err := d.Pool.QueryRow(ctx,
			`INSERT INTO manifests (total_size, chunk_count, content_hash)
			 VALUES (10, 1, $1) RETURNING id`, hash).Scan(m); err != nil {
			t.Fatalf("insert manifest: %v", err)
		}
	}
	for _, row := range []struct {
		m   uuid.UUID
		seq int
		off int64
	}{{m1, 0, 0}, {m1, 1, 10}, {m2, 0, 0}} {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO manifest_chunks (manifest_id, seq, chunk_hash, byte_offset)
			 VALUES ($1, $2, $3, $4)`, row.m, row.seq, hash, row.off); err != nil {
			t.Fatalf("insert manifest_chunk: %v", err)
		}
	}
	if got := chunkRefcount(t, d, hash); got != 3 {
		t.Fatalf("refcount after 3 references = %d, want 3", got)
	}

	// RESTRICT, not CASCADE: deleting a live chunk must be refused outright.
	if _, err := d.Pool.Exec(ctx, `DELETE FROM chunks WHERE hash = $1`, hash); err == nil {
		t.Fatal("deleted a chunk that two manifests still reference")
	}

	// Dropping a manifest cascades manifest_chunks away. No Go code names those
	// rows, which is exactly why the accounting is a trigger.
	if _, err := d.Pool.Exec(ctx, `DELETE FROM manifests WHERE id = $1`, m1); err != nil {
		t.Fatalf("delete manifest: %v", err)
	}
	if got := chunkRefcount(t, d, hash); got != 1 {
		t.Fatalf("refcount after cascading away 2 references = %d, want 1", got)
	}

	if _, err := d.Pool.Exec(ctx, `DELETE FROM manifests WHERE id = $1`, m2); err != nil {
		t.Fatalf("delete manifest: %v", err)
	}
	if got := chunkRefcount(t, d, hash); got != 0 {
		t.Fatalf("refcount after the last reference = %d, want 0", got)
	}

	// Only now is the chunk collectable.
	if _, err := d.Pool.Exec(ctx, `DELETE FROM chunks WHERE hash = $1`, hash); err != nil {
		t.Fatalf("delete unreferenced chunk: %v", err)
	}
}

// file_versions must point at exactly one of blob or manifest. The CHECK is
// what stops an application bug producing a version two readers disagree about.
func TestFileVersionStorageCheck(t *testing.T) {
	d := scratchDB(t)
	ctx := context.Background()

	if err := d.Migrate(ctx, quietLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	node, blobID, manifestID := seedNodeBlobManifest(t, d)

	t.Run("neither", func(t *testing.T) {
		_, err := d.Pool.Exec(ctx,
			`INSERT INTO file_versions (node_id, blob_id, manifest_id, size)
			 VALUES ($1, NULL, NULL, 10)`, node)
		if err == nil {
			t.Fatal("inserted a version pointing at nothing")
		}
	})
	t.Run("both", func(t *testing.T) {
		_, err := d.Pool.Exec(ctx,
			`INSERT INTO file_versions (node_id, blob_id, manifest_id, size)
			 VALUES ($1, $2, $3, 10)`, node, blobID, manifestID)
		if err == nil {
			t.Fatal("inserted a version pointing at both a blob and a manifest")
		}
	})
	t.Run("manifest only", func(t *testing.T) {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO file_versions (node_id, manifest_id, size)
			 VALUES ($1, $2, 10)`, node, manifestID); err != nil {
			t.Fatalf("manifest-backed version rejected: %v", err)
		}
	})
	t.Run("blob only", func(t *testing.T) {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO file_versions (node_id, blob_id, size)
			 VALUES ($1, $2, 10)`, node, blobID); err != nil {
			t.Fatalf("blob-backed version rejected: %v", err)
		}
	})
}

// --- helpers ----------------------------------------------------------------

func tableExists(t *testing.T, d *DB, name string) bool {
	t.Helper()
	var exists bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.tables
		     WHERE table_schema = 'public' AND table_name = $1
		 )`, name).Scan(&exists); err != nil {
		t.Fatalf("check table %q: %v", name, err)
	}
	return exists
}

func columnExists(t *testing.T, d *DB, table, column string) bool {
	t.Helper()
	var exists bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.columns
		     WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
		 )`, table, column).Scan(&exists); err != nil {
		t.Fatalf("check column %q.%q: %v", table, column, err)
	}
	return exists
}

func chunkRefcount(t *testing.T, d *DB, hash []byte) int64 {
	t.Helper()
	var n int64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT refcount FROM chunks WHERE hash = $1`, hash).Scan(&n); err != nil {
		t.Fatalf("read refcount: %v", err)
	}
	return n
}

// seedNodeBlobManifest creates the minimum tree a file version needs: a user,
// their root, a file node under it, plus one blob and one manifest to point at.
func seedNodeBlobManifest(t *testing.T, d *DB) (node, blobID, manifestID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	username := "mig-" + uuid.NewString()[:8]
	var user uuid.UUID
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO users (username, username_fold, display_name)
		 VALUES ($1, $1, $1) RETURNING id`, username).Scan(&user); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var root uuid.UUID
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO nodes (owner_id, parent_id, kind, name, name_fold, path)
		 VALUES ($1, NULL, 'folder', '', '', '/') RETURNING id`, user).Scan(&root); err != nil {
		t.Fatalf("insert root: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO nodes (owner_id, parent_id, kind, name, name_fold, path)
		 VALUES ($1, $2, 'file', 'a.txt', 'a.txt', '/a.txt') RETURNING id`,
		user, root).Scan(&node); err != nil {
		t.Fatalf("insert node: %v", err)
	}

	sum := make([]byte, 32)
	if _, err := rand.Read(sum); err != nil {
		t.Fatalf("random: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO blobs (storage_key, size, sha256) VALUES ($1, 10, $2) RETURNING id`,
		hex.EncodeToString(sum), sum).Scan(&blobID); err != nil {
		t.Fatalf("insert blob: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO manifests (total_size, chunk_count, content_hash)
		 VALUES (10, 0, $1) RETURNING id`, sum).Scan(&manifestID); err != nil {
		t.Fatalf("insert manifest: %v", err)
	}
	return node, blobID, manifestID
}

func seedManifestBackedVersion(t *testing.T, d *DB) {
	t.Helper()
	node, _, manifestID := seedNodeBlobManifest(t, d)
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO file_versions (node_id, manifest_id, size) VALUES ($1, $2, 10)`,
		node, manifestID); err != nil {
		t.Fatalf("insert manifest-backed version: %v", err)
	}
}

func seedBlobBackedVersion(t *testing.T, d *DB) {
	t.Helper()
	node, blobID, _ := seedNodeBlobManifest(t, d)
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO file_versions (node_id, blob_id, size) VALUES ($1, $2, 10)`,
		node, blobID); err != nil {
		t.Fatalf("insert blob-backed version: %v", err)
	}
}

// The race this guards: two processes migrating one fresh database both read
// "no version", both start issuing DDL, and one dies with "relation already
// exists" — which is what `go test ./...` does to the shared test database
// when two packages' fixtures start simultaneously. Migrate holds a Postgres
// advisory lock for the duration, so the loser waits and then no-ops.
//
// One process cannot truly impersonate several (an in-process mutex also
// serialises these goroutines), but this pins the behaviour the lock and the
// mutex must jointly provide: N concurrent Migrate calls on an empty database
// all succeed and the schema comes out at the latest version exactly once.
func TestConcurrentMigratorsSerialize(t *testing.T) {
	scratch := scratchDB(t)
	ctx := context.Background()

	const migrators = 4
	errs := make(chan error, migrators)
	for range migrators {
		go func() { errs <- scratch.Migrate(ctx, quietLog()) }()
	}
	for range migrators {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Migrate: %v", err)
		}
	}

	got, err := scratch.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if want := latestVersion(t); got != want {
		t.Fatalf("schema at version %d after concurrent migrators, want %d", got, want)
	}
}
