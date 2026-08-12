package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

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
	// ErrContentHashMismatch is a manifest commit whose named chunks do not
	// reassemble to the whole-file hash the client declared. Distinct from
	// ErrHashMismatch, which is about one chunk: this one is about the file they
	// compose, and the two have very different causes — a corrupt transfer versus
	// a client asserting an identity that is not its content's.
	ErrContentHashMismatch = errors.New("chunks do not reassemble to the declared content hash")
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

// OpenChunkStored opens a chunk's ON-DISK bytes for streaming, alongside its
// compression and plaintext size.
//
// The caller closes the reader. Used by the download path, which has no reason
// to hold a chunk in memory: it copies the bytes straight to the socket, and a
// hundred concurrent syncs would otherwise each own a buffer for as long as
// their client took to read it. ReadChunkStored keeps buffering for the callers
// that genuinely need the bytes in hand — hashing, verification.
func (s *Store) OpenChunkStored(ctx context.Context, hash [32]byte) (rc io.ReadCloser, compression string, size int, err error) {
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

	f, err := s.blobs.Open(ctx, storageKey)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, "", 0, ErrChunkNotFound
	}
	if err != nil {
		return nil, "", 0, err
	}
	return f, compression, size, nil
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

// verifyContentHash recomputes a file's whole-file BLAKE3 from the chunks a
// client named, in the order it named them.
//
// It reproduces exactly what the chunker does on the streaming path — hash the
// plaintext of every chunk in order, into one BLAKE3 — so the two upload paths
// agree on what a file's identity is. If they did not, an identical file
// uploaded two ways would fail to dedup, and worse, would carry two different
// keys into every derived table.
//
// Each chunk is re-verified against its own address on the way through. Its
// bytes were checked when they were stored, but a commit is precisely the moment
// a caller asserts what a file contains, and rot between the two would otherwise
// be blessed here as a valid file.
func (s *Store) verifyContentHash(ctx context.Context, expected [32]byte, hashes [][32]byte) error {
	whole := blake3.New()
	for _, h := range hashes {
		stored, compression, size, err := s.ReadChunkStored(ctx, h)
		if err != nil {
			return err
		}
		plain, err := Decompress(stored, compression, size)
		if err != nil {
			return err
		}
		if got := blake3.Sum256(plain); got != h {
			return fmt.Errorf("chunk %x failed verification: content hashes to %x", h, got)
		}
		if _, err := whole.Write(plain); err != nil {
			return err
		}
	}

	var got [32]byte
	copy(got[:], whole.Sum(nil))
	if got != expected {
		return ErrContentHashMismatch
	}
	return nil
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

// encoderPool reuses zstd encoders across single-chunk uploads. Each is created
// with concurrency 1 and used serially per Get/Put, so there is no sharing to
// race — and no per-chunk encoder allocation, which would dominate the cost of
// compressing 16 KiB.
var encoderPool = sync.Pool{
	New: func() any {
		e, _ := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderConcurrency(1))
		return e
	},
}

// compressChunk applies the same policy as the chunker: keep compression only
// when it wins by a clear margin, so already-compressed content is stored as-is
// rather than paying decompression on every read to save nothing.
func compressChunk(plain []byte) ([]byte, string) {
	enc := encoderPool.Get().(*zstd.Encoder)
	defer encoderPool.Put(enc)
	if c := enc.EncodeAll(plain, nil); len(c) < len(plain)*9/10 {
		return c, CompressionZstd
	}
	return bytes.Clone(plain), CompressionNone
}

// PutChunk stores one client-supplied plaintext chunk, addressed by hash.
//
// This is the endpoint the whole phase is careful about: the first path where a
// client writes content the server will address and dedup. So the server
// recomputes BLAKE3 and REFUSES any mismatch — a chunk stored under the wrong
// address corrupts every file that later dedups against it, across users, and
// "the client said so" is never sufficient for content addressing. Bytes land
// before the row, as everywhere; a crash between leaves an orphan fsck reclaims.
// Returns whether the chunk was newly stored (false means it was already present
// — the ordinary case once dedup is working).
func (s *Store) PutChunk(ctx context.Context, expected [32]byte, plain []byte) (bool, error) {
	if len(plain) == 0 || len(plain) > MaxChunkSize {
		return false, ErrChunkTooLarge
	}
	if blake3.Sum256(plain) != expected {
		return false, ErrHashMismatch
	}

	stored, compression := compressChunk(plain)
	key := StorageKey(expected)
	if _, err := s.putter.PutKeyed(ctx, key, bytes.NewReader(stored)); err != nil {
		return false, fmt.Errorf("store chunk: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO chunks (hash, size, stored_size, compression, storage_key)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (hash) DO NOTHING`,
		expected[:], len(plain), len(stored), compression, key)
	if err != nil {
		return false, fmt.Errorf("record chunk: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CommitManifest assembles an ordered list of ALREADY-STORED chunks into a
// manifest, ready for a file version to reference.
//
// Offsets and total size come from the chunks' own recorded sizes, never the
// client's claim: the client supplies the ORDER, and the server owns everything
// else — the geometry and, since verifyContentHash below, the identity. A
// referenced chunk that was never uploaded is rejected up front rather than as a
// raw foreign-key error. An identical live manifest is reused (whole-file
// dedup), exactly as streaming Write does. Returns the manifest and whether it
// was reused.
func (s *Store) CommitManifest(ctx context.Context, contentHash [32]byte, hashes [][32]byte) (*Manifest, bool, error) {
	if len(hashes) == 0 {
		return nil, false, ErrEmptyManifest
	}

	keys := make([][]byte, len(hashes))
	for i, h := range hashes {
		keys[i] = h[:]
	}
	rows, err := s.pool.Query(ctx, `SELECT hash, size FROM chunks WHERE hash = ANY($1)`, keys)
	if err != nil {
		return nil, false, err
	}
	sizeByHash := make(map[[32]byte]int, len(hashes))
	for rows.Next() {
		var (
			h  []byte
			sz int
		)
		if err := rows.Scan(&h, &sz); err != nil {
			rows.Close()
			return nil, false, err
		}
		var key [32]byte
		copy(key[:], h)
		sizeByHash[key] = sz
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	var total int64
	offsets := make([]int64, len(hashes))
	for i, h := range hashes {
		sz, ok := sizeByHash[h]
		if !ok {
			return nil, false, fmt.Errorf("%w: %x", ErrMissingChunk, h)
		}
		offsets[i] = total
		total += int64(sz)
	}
	chunkCount := len(hashes)

	// The client's whole-file hash is VERIFIED against the chunks it named,
	// before the reuse lookup below and before any row is written.
	//
	// Everything downstream treats content_hash as the identity of the bytes:
	// dedup joins on it, and doc_text, doc_embedding, media_meta and media_variant
	// are all keyed by it. Taking it on trust made it a capability rather than a
	// checksum. GET /nodes/{id}/manifest hands a file's hash, size and chunk count
	// to anyone who can read it — including a grantee whose access is later
	// revoked — and the reuse branch below matches on exactly those three, so a
	// caller could name any chunks summing to that size and be handed back the
	// EXISTING manifest, and with it the whole of somebody else's file.
	//
	// The chunks are already on local disk, so this is a local read, not a
	// transfer: the delta protocol still moves only what changed. PutChunk
	// verifies each chunk against its own address; this is the same rule applied
	// to the whole they compose, and it is the only place it can be applied.
	if err := s.verifyContentHash(ctx, contentHash, hashes); err != nil {
		return nil, false, err
	}

	// Reuse a live identical manifest. Only manifests a live version references
	// are candidates — an orphan may be mid-delete by GC.
	var existingID uuid.UUID
	err = s.pool.QueryRow(ctx, `
		SELECT m.id FROM manifests m
		WHERE m.content_hash = $1 AND m.total_size = $2 AND m.chunk_count = $3
		  AND EXISTS (SELECT 1 FROM file_versions v WHERE v.manifest_id = m.id)
		LIMIT 1`, contentHash[:], total, chunkCount).Scan(&existingID)
	if err == nil {
		return &Manifest{ID: existingID, TotalSize: total, ChunkCount: chunkCount, ContentHash: contentHash}, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("look up existing manifest: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var manifestID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO manifests (total_size, chunk_count, content_hash)
		VALUES ($1,$2,$3) RETURNING id`,
		total, chunkCount, contentHash[:]).Scan(&manifestID); err != nil {
		return nil, false, fmt.Errorf("create manifest: %w", err)
	}

	mcRows := make([][]any, len(hashes))
	for i, h := range hashes {
		mcRows[i] = []any{manifestID, i, h[:], offsets[i]}
	}
	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{"manifest_chunks"},
		[]string{"manifest_id", "seq", "chunk_hash", "byte_offset"},
		pgx.CopyFromRows(mcRows)); err != nil {
		return nil, false, fmt.Errorf("record manifest chunks: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return &Manifest{ID: manifestID, TotalSize: total, ChunkCount: chunkCount, ContentHash: contentHash}, false, nil
}
