package cas

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
)

// The delta protocol: the primitives a sync client uses to transfer only what
// changed. A client fetches a file's chunk list, learns which chunks it lacks,
// pulls those, and — going the other way — asks which of a file's chunks the
// server already has and uploads only the rest. CAS turns "sync a 4 GB file that
// changed by one block" into "move one block".

var (
	// ErrHashMismatch is the load-bearing error of this whole phase: a client
	// uploaded bytes that do not hash to the address it claimed. Content
	// addressing is only trustworthy because the server refuses to take the
	// client's word for it.
	ErrHashMismatch = errors.New("chunk content does not match its hash")
	// ErrChunkTooLarge rejects an upload larger than a chunk can legitimately be.
	ErrChunkTooLarge = errors.New("chunk exceeds the maximum chunk size")
	// ErrEmptyManifest rejects a commit with no chunks; a file that small is a
	// whole-file blob, not a manifest.
	ErrEmptyManifest = errors.New("a manifest must reference at least one chunk")
	// ErrMissingChunk is a manifest commit referencing a chunk the server does not
	// have — the client must upload it first.
	ErrMissingChunk = errors.New("manifest references a chunk that is not stored")
	// ErrChunkNotFound is a read of a chunk that is not stored.
	ErrChunkNotFound = errors.New("chunk not found")
)

// Entry is one chunk of a manifest as the delta protocol exposes it: the address
// the client dedups on, and where the chunk sits in the file.
type Entry struct {
	Hash   [32]byte
	Offset int64
	Size   int
}

// Entries returns a manifest's full ordered chunk list.
//
// The whole list, unlike the reader's offset-bounded chunksFrom: a client
// diffing a file against what it already holds needs every hash, not the window
// covering a byte range.
func (s *Store) Entries(ctx context.Context, manifestID uuid.UUID) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT mc.chunk_hash, mc.byte_offset, c.size
		FROM manifest_chunks mc
		JOIN chunks c ON c.hash = mc.chunk_hash
		WHERE mc.manifest_id = $1
		ORDER BY mc.seq`, manifestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var (
			e    Entry
			hash []byte
		)
		if err := rows.Scan(&hash, &e.Offset, &e.Size); err != nil {
			return nil, err
		}
		copy(e.Hash[:], hash)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReadChunkStored returns a chunk's ON-DISK bytes, its compression, and its
// plaintext size — the stored form, so the wire carries the compressed bytes and
// the client decompresses and verifies against the address itself.
func (s *Store) ReadChunkStored(ctx context.Context, hash [32]byte) (stored []byte, compression string, size int, err error) {
	var storageKey string
	err = s.pool.QueryRow(ctx,
		`SELECT storage_key, compression, size FROM chunks WHERE hash = $1`, hash[:]).
		Scan(&storageKey, &compression, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", 0, ErrChunkNotFound
	}
	if err != nil {
		return nil, "", 0, err
	}

	rc, err := s.blobs.Open(ctx, storageKey)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, "", 0, ErrChunkNotFound
	}
	if err != nil {
		return nil, "", 0, err
	}
	defer rc.Close()

	stored, err = io.ReadAll(rc)
	if err != nil {
		return nil, "", 0, err
	}
	return stored, compression, size, nil
}

// HasChunks reports which of the given hashes are already stored, so a client
// uploads only the chunks the server lacks.
func (s *Store) HasChunks(ctx context.Context, hashes [][32]byte) (map[[32]byte]bool, error) {
	keys := make([][]byte, len(hashes))
	for i, h := range hashes {
		keys[i] = h[:]
	}
	rows, err := s.pool.Query(ctx,
		`SELECT hash FROM chunks WHERE hash = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	present := make(map[[32]byte]bool, len(hashes))
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		var key [32]byte
		copy(key[:], h)
		present[key] = true
	}
	return present, rows.Err()
}
