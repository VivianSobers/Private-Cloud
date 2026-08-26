package cas

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ChunkRef identifies a stored chunk for the garbage collector and fsck.
type ChunkRef struct {
	Hash       [32]byte
	StorageKey string
	Size       int
	StoredSize int
	// Tier is 'hot' or 'cold' (migration 00028). Populated by AllChunkKeys, for
	// fsck; the GC paths leave it empty because deleting a chunk deletes it from
	// both tiers and does not need to know which one held it.
	Tier string
}

// AllChunkKeys returns every storage key the chunk table knows about.
//
// fsck needs this because chunks and whole-file blobs share one blob store
// root. Without it, every chunk on disk looks like an orphan to a checker that
// only knows about `blobs` — and `--repair` would then delete all deduplicated
// content in the system.
func (s *Store) AllChunkKeys(ctx context.Context) (map[string]ChunkRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT hash, storage_key, size, stored_size, tier FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]ChunkRef)
	for rows.Next() {
		var (
			c    ChunkRef
			hash []byte
		)
		if err := rows.Scan(&hash, &c.StorageKey, &c.Size, &c.StoredSize, &c.Tier); err != nil {
			return nil, err
		}
		copy(c.Hash[:], hash)
		out[c.StorageKey] = c
	}
	return out, rows.Err()
}

// UnreferencedChunks lists chunks no manifest points at any more.
//
// The grace period is not cosmetic. Chunk rows are written before the manifest
// that references them, so an upload in flight legitimately sits at refcount
// zero for the length of its transaction — and a GC that ignored that would
// delete the chunks of a file being written right now.
func (s *Store) UnreferencedChunks(ctx context.Context, grace time.Duration, limit int) ([]ChunkRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT hash, storage_key, size, stored_size FROM chunks
		WHERE refcount = 0 AND created_at < now() - $1::interval
		ORDER BY created_at
		LIMIT $2`, grace.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChunkRef
	for rows.Next() {
		var (
			c    ChunkRef
			hash []byte
		)
		if err := rows.Scan(&hash, &c.StorageKey, &c.Size, &c.StoredSize); err != nil {
			return nil, err
		}
		copy(c.Hash[:], hash)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteChunkRow removes the row, but only if it is STILL unreferenced.
//
// The refcount condition closes the window between listing a chunk and deleting
// it, in which another upload could have deduplicated against it. That window
// is not theoretical: dedup means popular chunks are claimed constantly, and
// one chunk is shared across files belonging to different people — so losing
// this race would corrupt a file for someone who was never involved.
func (s *Store) DeleteChunkRow(ctx context.Context, hash [32]byte) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM chunks WHERE hash = $1 AND refcount = 0`, hash[:])
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UnreferencedManifests lists manifests no file version points at any more.
//
// Purging a trashed file deletes its version rows; the manifest survives
// because nothing cascades to it (file_versions -> manifests is RESTRICT, and
// RESTRICT guards the other direction). This is the pass that reaps them.
// Deleting a manifest cascades its manifest_chunks rows, and the refcount
// trigger walks each chunk down — which is what feeds UnreferencedChunks on
// the next pass. The chain is: version rows -> manifests -> chunk rows ->
// chunk bytes, each step reclaimed by the step before it.
//
// The grace period covers the write ordering: cas.Write commits a manifest
// BEFORE PutFile inserts the version referencing it, so an upload in flight
// legitimately owns an unreferenced manifest for the length of that gap.
func (s *Store) UnreferencedManifests(ctx context.Context, grace time.Duration, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id FROM manifests m
		WHERE NOT EXISTS (SELECT 1 FROM file_versions v WHERE v.manifest_id = m.id)
		  AND m.created_at < now() - $1::interval
		ORDER BY m.created_at
		LIMIT $2`, grace.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteManifestRow removes a manifest, but only if it is STILL unreferenced.
//
// The NOT EXISTS re-check inside the delete closes the window in which an
// identical upload could have reused this manifest between listing and
// deletion — manifest reuse makes that race real, not hypothetical, and losing
// it would sever a freshly uploaded file from its content.
func (s *Store) DeleteManifestRow(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM manifests m
		WHERE m.id = $1
		  AND NOT EXISTS (SELECT 1 FROM file_versions v WHERE v.manifest_id = m.id)`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RefcountDrift is a chunk whose stored refcount disagrees with reality.
type RefcountDrift struct {
	Hash     [32]byte
	Recorded int64
	Actual   int64
}

// VerifyRefcounts recomputes every chunk's reference count from
// manifest_chunks and reports disagreements.
//
// Read-only, deliberately. The trigger should make drift impossible, so drift
// means something is already wrong — a bug, a manual edit, a partially applied
// migration. Silently "fixing" the number would destroy the evidence and could
// just as easily make things worse: an inflated count wastes disk, but a
// wrongly deflated one gets live chunks deleted.
func (s *Store) VerifyRefcounts(ctx context.Context, limit int) ([]RefcountDrift, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.hash, c.refcount, coalesce(m.n, 0)
		FROM chunks c
		LEFT JOIN (
			SELECT chunk_hash, count(*) AS n FROM manifest_chunks GROUP BY chunk_hash
		) m ON m.chunk_hash = c.hash
		WHERE c.refcount <> coalesce(m.n, 0)
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("verify refcounts: %w", err)
	}
	defer rows.Close()

	var out []RefcountDrift
	for rows.Next() {
		var (
			d    RefcountDrift
			hash []byte
		)
		if err := rows.Scan(&hash, &d.Recorded, &d.Actual); err != nil {
			return nil, err
		}
		copy(d.Hash[:], hash)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Stats describes what dedup and compression are actually saving.
type Stats struct {
	Chunks int64
	// LogicalBytes is the sum of every chunk reference — what the stored files
	// weigh as their owners understand them.
	LogicalBytes int64
	// UniqueBytes is the plaintext size of distinct chunks: logical minus what
	// dedup saved.
	UniqueBytes int64
	// PhysicalBytes is what is actually on disk, after compression too.
	PhysicalBytes int64
	Manifests     int64
}

func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	st := &Stats{}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(size),0), coalesce(sum(stored_size),0) FROM chunks`).
		Scan(&st.Chunks, &st.UniqueBytes, &st.PhysicalBytes); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(total_size),0) FROM manifests`).
		Scan(&st.Manifests, &st.LogicalBytes); err != nil {
		return nil, err
	}
	return st, nil
}
