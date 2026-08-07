package files

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Media metadata and derived variants, content-addressed like doc_text and
// doc_embedding. See migration 00019 for why.

// MediaMeta is what was determined about one file's bytes.
type MediaMeta struct {
	Width, Height int
	Orientation   int
	TakenAt       *time.Time
	Camera        string
	GPSLat        *float64
	GPSLon        *float64
	DurationMS    *int64
	Source        string
	// Variants names the renditions that exist RIGHT NOW, so a gallery can tell
	// "not generated yet" from "will never exist" without a request per tile.
	Variants []string
}

// MediaVariant locates one rendition's stored bytes.
type MediaVariant struct {
	Variant       string
	StorageKey    string
	MIME          string
	Size          int64
	Width, Height int
}

// PutMediaMeta records a file's media metadata, replacing any prior row for the
// same content. Idempotent by content hash, so a job that runs twice — or two
// files with identical bytes — write the same row.
func (s *Store) PutMediaMeta(ctx context.Context, contentHash []byte, m MediaMeta) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO media_meta
			(content_hash, width, height, orientation, taken_at, camera,
			 gps_lat, gps_lon, duration_ms, source, extracted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		ON CONFLICT (content_hash) DO UPDATE SET
			width = excluded.width, height = excluded.height,
			orientation = excluded.orientation, taken_at = excluded.taken_at,
			camera = excluded.camera, gps_lat = excluded.gps_lat,
			gps_lon = excluded.gps_lon, duration_ms = excluded.duration_ms,
			source = excluded.source, extracted_at = now()`,
		contentHash, nullIfZero(m.Width), nullIfZero(m.Height), orientationOr1(m.Orientation),
		m.TakenAt, m.Camera, m.GPSLat, m.GPSLon, m.DurationMS, m.Source)
	return err
}

// HasMediaMeta reports whether this content has already been analysed, so the
// worker can skip bytes it has seen before under a different filename.
func (s *Store) HasMediaMeta(ctx context.Context, contentHash []byte) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM media_meta WHERE content_hash = $1)`, contentHash).Scan(&exists)
	return exists, err
}

// MediaMetaFor returns one content hash's metadata, including which variants
// currently exist.
func (s *Store) MediaMetaFor(ctx context.Context, contentHash []byte) (*MediaMeta, bool, error) {
	var (
		m             MediaMeta
		width, height *int
		variants      []string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT mm.width, mm.height, mm.orientation, mm.taken_at, mm.camera,
		       mm.gps_lat, mm.gps_lon, mm.duration_ms, mm.source,
		       coalesce(array_agg(mv.variant ORDER BY mv.variant)
		                FILTER (WHERE mv.variant IS NOT NULL), '{}')
		FROM media_meta mm
		LEFT JOIN media_variant mv ON mv.content_hash = mm.content_hash
		WHERE mm.content_hash = $1
		GROUP BY mm.content_hash`, contentHash).
		Scan(&width, &height, &m.Orientation, &m.TakenAt, &m.Camera,
			&m.GPSLat, &m.GPSLon, &m.DurationMS, &m.Source, &variants)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if width != nil {
		m.Width = *width
	}
	if height != nil {
		m.Height = *height
	}
	m.Variants = variants
	return &m, true, nil
}

// MediaMetaForNode returns a node's media metadata, verifying ownership. Used by
// the single-node GET so a details view needs one request.
func (s *Store) MediaMetaForNode(ctx context.Context, ownerID, nodeID uuid.UUID) (*MediaMeta, bool, error) {
	var hash []byte
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(b.sha256, m.content_hash)
		FROM nodes n
		JOIN file_versions v ON v.id = n.head_version_id
		LEFT JOIN blobs b     ON b.id = v.blob_id
		LEFT JOIN manifests m ON m.id = v.manifest_id
		WHERE n.id = $1 AND n.owner_id = $2 AND n.trashed_at IS NULL`,
		nodeID, ownerID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(hash) == 0 {
		return nil, false, nil
	}
	return s.MediaMetaFor(ctx, hash)
}

// PutMediaVariant records a rendered variant. Idempotent by (content, variant):
// re-rendering replaces the row, and the caller deletes the superseded blob.
// It returns the storage key it displaced, or "" if there was none. The caller
// deletes those bytes — this package does not, because the row must be updated
// before the file disappears, never after.
//
// The old key is read through a CTE rather than a subselect in RETURNING. A CTE
// is evaluated against the snapshot taken at the start of the statement, so it
// reliably sees the PRE-update row; a subselect inside RETURNING alongside an
// ON CONFLICT DO UPDATE is subtle enough that the difference between reading the
// old and the new key is not obvious from the text, and getting it wrong here
// means either leaking a blob forever or deleting the one just written.
func (s *Store) PutMediaVariant(ctx context.Context, contentHash []byte, v MediaVariant) (replacedKey string, err error) {
	err = s.pool.QueryRow(ctx, `
		WITH old AS (
			SELECT storage_key FROM media_variant
			WHERE content_hash = $1 AND variant = $2
		), upserted AS (
			INSERT INTO media_variant
				(content_hash, variant, storage_key, mime, size, width, height)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (content_hash, variant) DO UPDATE SET
				storage_key = excluded.storage_key, mime = excluded.mime,
				size = excluded.size, width = excluded.width, height = excluded.height,
				created_at = now()
			RETURNING 1
		)
		SELECT coalesce((SELECT storage_key FROM old), '')
		FROM (SELECT 1 FROM upserted) _`,
		contentHash, v.Variant, v.StorageKey, v.MIME, v.Size, v.Width, v.Height).Scan(&replacedKey)
	if err != nil {
		return "", err
	}
	// Keys are content-addressed, so re-rendering identical bytes lands on the
	// same key. Nothing was displaced, and deleting it would delete the live one.
	if replacedKey == v.StorageKey {
		return "", nil
	}
	return replacedKey, nil
}

// MediaVariantFor locates a variant's bytes for one of the owner's live files.
//
// Ownership is checked in the same query as the lookup, not before it: a variant
// is derived from content, and content is shared between users by dedup, so the
// only thing that makes this file's thumbnail THIS caller's to read is the node
// row joining them.
func (s *Store) MediaVariantFor(ctx context.Context, ownerID, nodeID uuid.UUID, variant string) (*MediaVariant, error) {
	var v MediaVariant
	err := s.pool.QueryRow(ctx, `
		SELECT mv.variant, mv.storage_key, mv.mime, mv.size, mv.width, mv.height
		FROM nodes n
		JOIN file_versions fv ON fv.id = n.head_version_id
		LEFT JOIN blobs b     ON b.id = fv.blob_id
		LEFT JOIN manifests m ON m.id = fv.manifest_id
		JOIN media_variant mv ON mv.content_hash = coalesce(b.sha256, m.content_hash)
		WHERE n.id = $1 AND n.owner_id = $2 AND n.trashed_at IS NULL AND mv.variant = $3`,
		nodeID, ownerID, variant).
		Scan(&v.Variant, &v.StorageKey, &v.MIME, &v.Size, &v.Width, &v.Height)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// PruneMediaMeta deletes metadata for content no live version references,
// bounded. Same contract as PruneDocText.
func (s *Store) PruneMediaMeta(ctx context.Context, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM media_meta
		WHERE content_hash IN (
			SELECT mm.content_hash FROM media_meta mm
			WHERE NOT EXISTS (
				SELECT 1 FROM file_versions v
				LEFT JOIN blobs b     ON b.id = v.blob_id
				LEFT JOIN manifests m ON m.id = v.manifest_id
				WHERE coalesce(b.sha256, m.content_hash) = mm.content_hash
			)
			LIMIT $1
		)`, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// UnreferencedMediaVariants lists variants whose content no live version
// references, so GC can delete the rows and then their blobs.
//
// Returns the rows rather than deleting them, because the bytes have to go too
// and the row must go FIRST — the same ordering blob GC uses. A row without its
// file is an orphan fsck can sweep; a file without its row is a dangling
// reference that breaks a request.
func (s *Store) UnreferencedMediaVariants(ctx context.Context, limit int) ([]MediaVariant, [][]byte, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT mv.content_hash, mv.variant, mv.storage_key, mv.mime, mv.size, mv.width, mv.height
		FROM media_variant mv
		WHERE NOT EXISTS (
			SELECT 1 FROM file_versions v
			LEFT JOIN blobs b     ON b.id = v.blob_id
			LEFT JOIN manifests m ON m.id = v.manifest_id
			WHERE coalesce(b.sha256, m.content_hash) = mv.content_hash
		)
		LIMIT $1`, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var (
		out    []MediaVariant
		hashes [][]byte
	)
	for rows.Next() {
		var (
			v    MediaVariant
			hash []byte
		)
		if err := rows.Scan(&hash, &v.Variant, &v.StorageKey, &v.MIME, &v.Size, &v.Width, &v.Height); err != nil {
			return nil, nil, err
		}
		out = append(out, v)
		hashes = append(hashes, hash)
	}
	return out, hashes, rows.Err()
}

// DeleteMediaVariantRow removes one variant row, reporting whether it was still
// there. Re-checks that the content is still unreferenced, because a new upload
// of identical bytes between the list and now must not lose its thumbnail.
func (s *Store) DeleteMediaVariantRow(ctx context.Context, contentHash []byte, variant string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM media_variant mv
		WHERE mv.content_hash = $1 AND mv.variant = $2
		  AND NOT EXISTS (
			SELECT 1 FROM file_versions v
			LEFT JOIN blobs b     ON b.id = v.blob_id
			LEFT JOIN manifests m ON m.id = v.manifest_id
			WHERE coalesce(b.sha256, m.content_hash) = mv.content_hash
		  )`, contentHash, variant)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MediaVariantKeys returns every storage key media variants reference, for fsck.
// Without this, fsck walks the blob store, finds thumbnails it cannot account
// for, and --repair deletes every one of them.
func (s *Store) MediaVariantKeys(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.pool.Query(ctx, `SELECT storage_key FROM media_variant`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = struct{}{}
	}
	return out, rows.Err()
}

func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func orientationOr1(n int) int {
	if n < 1 || n > 8 {
		return 1
	}
	return n
}
