package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
)

var ErrManifestNotFound = errors.New("manifest not found")

// Store persists chunks and manifests.
type Store struct {
	pool   *pgxpool.Pool
	blobs  blob.Store
	putter blob.KeyedPutter
}

// NewStore returns nil if the blob store cannot address content by key, which
// is what CAS fundamentally requires.
func NewStore(pool *pgxpool.Pool, blobs blob.Store) (*Store, error) {
	putter, ok := blobs.(blob.KeyedPutter)
	if !ok {
		return nil, errors.New("this blob store cannot address content by key")
	}
	return &Store{pool: pool, blobs: blobs, putter: putter}, nil
}

// Manifest describes a stored file.
type Manifest struct {
	ID          uuid.UUID
	TotalSize   int64
	ChunkCount  int
	ContentHash [32]byte
}

// WriteResult reports what a write cost.
type WriteResult struct {
	Manifest *Manifest

	// LogicalSize is what the user thinks they stored — the file's size. Quota
	// counts this, never the physical figure: charging someone less because
	// another user happens to own the same file would be unpredictable and
	// would leak the existence of that other content.
	LogicalSize int64

	// NewBytes is what actually reached the disk after dedup and compression.
	// Reporting only, for the storage-savings statistics.
	NewBytes int64
	// DedupedChunks were already present; NewChunks were written.
	DedupedChunks int
	NewChunks     int
	// ReusedManifest reports whole-file dedup: an identical file was already
	// stored, so no new manifest rows were written at all.
	ReusedManifest bool
}

// Write chunks r, stores what is new, and records a manifest.
//
// Ordering matches the whole-file path and for the same reason: every chunk's
// BYTES are on disk before the manifest that references them is committed. A
// crash between the two leaves unreferenced chunks, which GC reclaims. The
// reverse would leave a manifest with a hole in the middle — a file that lists,
// describes and downloads as garbage, which no background job can repair.
func (s *Store) Write(ctx context.Context, r io.Reader) (*WriteResult, error) {
	type placed struct {
		hash        [32]byte
		size        int
		storedSize  int
		compression string
		offset      int64
	}

	var (
		chunks    []placed
		res       WriteResult
		totalSize int64
	)

	// Each chunk's bytes land on disk inside this callback, before the manifest
	// that references them exists. Streaming rather than accumulating keeps
	// memory flat regardless of file size.
	contentHash, err := Split(ctx, r, func(ch *Chunk) error {
		key := StorageKey(ch.Hash)
		existed, err := s.putter.PutKeyed(ctx, key, bytes.NewReader(ch.Stored))
		if err != nil {
			return fmt.Errorf("store chunk: %w", err)
		}
		if existed {
			res.DedupedChunks++
		} else {
			res.NewChunks++
			res.NewBytes += int64(len(ch.Stored))
		}

		chunks = append(chunks, placed{
			hash: ch.Hash, size: ch.Size, storedSize: len(ch.Stored),
			compression: ch.Compression, offset: ch.Offset,
		})
		totalSize += int64(ch.Size)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Whole-file dedup: an identical file already stored means an existing
	// manifest describes exactly these chunks (chunking is deterministic, so
	// same content implies the same chunk sequence). Reusing it skips the
	// manifest and manifest_chunks writes entirely.
	//
	// Only manifests referenced by a live file_version are candidates. An
	// orphaned manifest may be mid-delete by GC, and handing its ID to a caller
	// about to insert a version row would turn that race into a foreign-key
	// error. Referenced manifests are ones GC will not touch.
	//
	// Skipping the chunk-row upserts is safe on this path: a live manifest
	// references every one of these hashes through manifest_chunks, whose
	// RESTRICT foreign key guarantees the chunk rows exist. The bytes were
	// already (re)written by the Split callback above. Losing this lookup to a
	// race merely costs a duplicate manifest, which the schema tolerates.
	var existingID uuid.UUID
	err = s.pool.QueryRow(ctx, `
		SELECT m.id FROM manifests m
		WHERE m.content_hash = $1 AND m.total_size = $2 AND m.chunk_count = $3
		  AND EXISTS (SELECT 1 FROM file_versions v WHERE v.manifest_id = m.id)
		LIMIT 1`, contentHash[:], totalSize, len(chunks)).Scan(&existingID)
	if err == nil {
		res.Manifest = &Manifest{
			ID: existingID, TotalSize: totalSize,
			ChunkCount: len(chunks), ContentHash: contentHash,
		}
		res.LogicalSize = totalSize
		res.ReusedManifest = true
		return &res, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("look up existing manifest: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Upsert the chunk rows. ON CONFLICT DO NOTHING rather than an existence
	// check: two concurrent uploads of the same content race here constantly
	// once dedup is working, and losing that race must be a no-op rather than
	// an error.
	for _, c := range chunks {
		if _, err := tx.Exec(ctx, `
			INSERT INTO chunks (hash, size, stored_size, compression, storage_key)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (hash) DO NOTHING`,
			c.hash[:], c.size, c.storedSize, c.compression, StorageKey(c.hash)); err != nil {
			return nil, fmt.Errorf("record chunk: %w", err)
		}
	}

	var manifestID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO manifests (total_size, chunk_count, content_hash)
		VALUES ($1,$2,$3) RETURNING id`,
		totalSize, len(chunks), contentHash[:]).Scan(&manifestID); err != nil {
		return nil, fmt.Errorf("create manifest: %w", err)
	}

	// CopyFrom rather than a loop: a 4 GB file is ~250k rows, and round-tripping
	// each one would dominate the upload.
	rows := make([][]any, 0, len(chunks))
	for i, c := range chunks {
		rows = append(rows, []any{manifestID, i, c.hash[:], c.offset})
	}
	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{"manifest_chunks"},
		[]string{"manifest_id", "seq", "chunk_hash", "byte_offset"},
		pgx.CopyFromRows(rows)); err != nil {
		return nil, fmt.Errorf("record manifest chunks: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	res.Manifest = &Manifest{
		ID: manifestID, TotalSize: totalSize,
		ChunkCount: len(chunks), ContentHash: contentHash,
	}
	res.LogicalSize = totalSize
	return &res, nil
}

func (s *Store) GetManifest(ctx context.Context, id uuid.UUID) (*Manifest, error) {
	m := &Manifest{ID: id}
	var hash []byte
	err := s.pool.QueryRow(ctx,
		`SELECT total_size, chunk_count, content_hash FROM manifests WHERE id = $1`, id).
		Scan(&m.TotalSize, &m.ChunkCount, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrManifestNotFound
	}
	if err != nil {
		return nil, err
	}
	copy(m.ContentHash[:], hash)
	return m, nil
}

// chunkRef is one entry of a manifest, as the reader needs it.
type chunkRef struct {
	hash        [32]byte
	storageKey  string
	size        int
	compression string
	offset      int64
}

// chunksFrom loads the manifest entries covering byte offsets >= from.
//
// Bounded by the offset rather than loading the whole list: a Range request for
// the last megabyte of a 4 GB file should not materialise 250,000 rows to find
// the handful it needs.
func (s *Store) chunksFrom(ctx context.Context, manifestID uuid.UUID, from int64) ([]chunkRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.hash, c.storage_key, c.size, c.compression, mc.byte_offset
		FROM manifest_chunks mc
		JOIN chunks c ON c.hash = mc.chunk_hash
		WHERE mc.manifest_id = $1 AND mc.byte_offset + c.size > $2
		ORDER BY mc.seq`, manifestID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []chunkRef
	for rows.Next() {
		var (
			cr   chunkRef
			hash []byte
		)
		if err := rows.Scan(&hash, &cr.storageKey, &cr.size, &cr.compression, &cr.offset); err != nil {
			return nil, err
		}
		copy(cr.hash[:], hash)
		out = append(out, cr)
	}
	return out, rows.Err()
}
