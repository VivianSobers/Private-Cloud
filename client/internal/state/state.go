// Package state is the client's local sync database: per path, the identity of
// the version it last synced. This baseline — not the filesystem mtime alone,
// which lies across restores and clock changes — is what both local edits and
// remote changes are judged against. A file whose recorded hash still matches the
// server's has not changed remotely; a file whose size and mtime still match the
// record has not changed locally; anything else is a real event to reconcile.
//
// SQLite via modernc.org/sqlite, a pure-Go driver, so the client needs no C
// toolchain to build for whatever laptop it lands on.
package state

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// Entry records one synced node.
//
// Hash is the version identity: the server's blake3 (chunked) or sha256 (whole)
// for a file, empty for a folder. Size and MtimeUnix are the local file's shape
// at sync time, the cheap check that says "the bytes on disk are still the ones
// we synced" without re-hashing every file on every pass.
type Entry struct {
	Path      string // server-relative, always begins with "/"
	NodeID    string
	Kind      string // "file" or "folder"
	Size      int64
	MtimeUnix int64
	Hash      string
}

// Store is the local state database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS synced (
	path       TEXT PRIMARY KEY,
	node_id    TEXT NOT NULL,
	kind       TEXT NOT NULL,
	size       INTEGER NOT NULL DEFAULT 0,
	mtime_unix INTEGER NOT NULL DEFAULT 0,
	hash       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// Open opens (creating if absent) the state database at path.
func Open(path string) (*Store, error) {
	// WAL keeps a reader (the rescan) from blocking the writer (apply/push), and
	// busy_timeout absorbs the brief overlap between the two loops rather than
	// failing a write outright.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// One connection: the sync loops are serialized anyway, and a single writer
	// sidesteps SQLite's write-lock contention entirely.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Get returns the recorded entry for a path, or ok=false if none.
func (s *Store) Get(path string) (Entry, bool, error) {
	var e Entry
	err := s.db.QueryRow(
		`SELECT path, node_id, kind, size, mtime_unix, hash FROM synced WHERE path = ?`, path).
		Scan(&e.Path, &e.NodeID, &e.Kind, &e.Size, &e.MtimeUnix, &e.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	return e, true, nil
}

// Put inserts or replaces an entry.
func (s *Store) Put(e Entry) error {
	_, err := s.db.Exec(`
		INSERT INTO synced (path, node_id, kind, size, mtime_unix, hash)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			node_id = excluded.node_id, kind = excluded.kind,
			size = excluded.size, mtime_unix = excluded.mtime_unix, hash = excluded.hash`,
		e.Path, e.NodeID, e.Kind, e.Size, e.MtimeUnix, e.Hash)
	return err
}

// Delete removes an entry by path. Removing an absent path is not an error — a
// reconciliation may try to forget something it already forgot.
func (s *Store) Delete(path string) error {
	_, err := s.db.Exec(`DELETE FROM synced WHERE path = ?`, path)
	return err
}

// List returns every recorded entry, ordered by path so a caller can process
// parents before children (folders sort before their contents).
func (s *Store) List() ([]Entry, error) {
	rows, err := s.db.Query(
		`SELECT path, node_id, kind, size, mtime_unix, hash FROM synced ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Path, &e.NodeID, &e.Kind, &e.Size, &e.MtimeUnix, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Empty reports whether nothing has ever been synced — the signal that an
// initial full sync is needed rather than an incremental one.
func (s *Store) Empty() (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM synced`).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// --- cursor -----------------------------------------------------------------

const cursorKey = "change_cursor"

// Cursor returns the change-journal seq the client has applied up to, or 0 if it
// has never synced.
func (s *Store) Cursor() (int64, error) {
	var v int64
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, cursorKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// SetCursor records the change-journal position. Written after the changes up to
// it have been applied, so a crash resumes from the last fully applied seq rather
// than skipping the ones in flight.
func (s *Store) SetCursor(seq int64) error {
	_, err := s.db.Exec(`
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, cursorKey, seq)
	return err
}
