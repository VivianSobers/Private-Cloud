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

// MediaState reports what is already stored for one content hash: whether it has
// been analysed, the dimensions that were found, and which variants exist.
//
// One query rather than the bare "has it been analysed" this replaced. That
// question was not enough to decide whether there was work to do: variants are
// rendered after the metadata row and are best effort, so a failed render leaves
// metadata present and thumbnails absent, and answering only the first half made
// that state permanent — every retry, and every `cloudctl jobs reindex
// --kind=media`, stopped at the same early return.
func (s *Store) MediaState(ctx context.Context, contentHash []byte) (hasMeta bool, width, height int, variants []string, err error) {
	var w, h *int
	err = s.pool.QueryRow(ctx, `
		SELECT mm.width, mm.height,
		       coalesce(array_agg(mv.variant ORDER BY mv.variant)
		                FILTER (WHERE mv.variant IS NOT NULL), '{}')
		FROM media_meta mm
		LEFT JOIN media_variant mv ON mv.content_hash = mm.content_hash
		WHERE mm.content_hash = $1
		GROUP BY mm.content_hash`, contentHash).Scan(&w, &h, &variants)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, 0, nil, nil
	}
	if err != nil {
		return false, 0, 0, nil, err
	}
	if w != nil {
		width = *w
	}
	if h != nil {
		height = *h
	}
	return true, width, height, variants, nil
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

// MediaMetaForNode returns a node's media metadata for a caller who may READ the
// node — which since Phase 7 is not the same as owning it.
//
// Access goes through AccessFor, the single authorisation question, rather than
// through an owner_id filter spliced into this query. The download path became
// grant-aware and these did not, so a grantee could fetch a shared photo's full
// bytes and was told the file had no media metadata at all — no dimensions for
// the grid to lay out, no capture date, and no list of variants, which is what a
// tile reads to decide whether a thumbnail exists.
func (s *Store) MediaMetaForNode(ctx context.Context, userID, nodeID uuid.UUID) (*MediaMeta, bool, error) {
	if _, err := s.AccessFor(ctx, userID, nodeID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}

	var hash []byte
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(b.sha256, m.content_hash)
		FROM nodes n
		JOIN file_versions v ON v.id = n.head_version_id
		LEFT JOIN blobs b     ON b.id = v.blob_id
		LEFT JOIN manifests m ON m.id = v.manifest_id
		WHERE n.id = $1 AND n.trashed_at IS NULL`, nodeID).Scan(&hash)
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

// MediaVariantFor locates a variant's bytes for a caller who may read the node.
//
// A variant is derived from content, and content is shared between users by
// dedup, so knowing a content hash grants nothing: the NODE row is the only
// thing that makes a thumbnail this caller's to read. That check is AccessFor,
// the same question the download path asks, rather than an owner_id filter — a
// grantee who can download a shared photo in full but gets a 404 for its
// thumbnail is not a share, it is a broken gallery quietly pulling originals.
func (s *Store) MediaVariantFor(ctx context.Context, userID, nodeID uuid.UUID, variant string) (*MediaVariant, error) {
	if _, err := s.AccessFor(ctx, userID, nodeID); err != nil {
		return nil, err
	}

	var v MediaVariant
	err := s.pool.QueryRow(ctx, `
		SELECT mv.variant, mv.storage_key, mv.mime, mv.size, mv.width, mv.height
		FROM nodes n
		JOIN file_versions fv ON fv.id = n.head_version_id
		LEFT JOIN blobs b     ON b.id = fv.blob_id
		LEFT JOIN manifests m ON m.id = fv.manifest_id
		JOIN media_variant mv ON mv.content_hash = coalesce(b.sha256, m.content_hash)
		WHERE n.id = $1 AND n.trashed_at IS NULL AND mv.variant = $2`,
		nodeID, variant).
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

// TimelineNodes returns the owner's media, newest first by capture time.
//
// Kept separate from search because it sorts by when the shutter fired and pages
// by date rather than by relevance. The JOIN against media_meta is what makes it
// a *media* timeline: a file with no analysed metadata is not a photo as far as
// this view is concerned, which is also why the backfill in `cloudctl jobs
// reindex --kind=media` matters — without it a pre-existing library is invisible
// here.
//
// The sort falls back to updated_at when taken_at is absent. The API response
// still omits taken_at in that case, so the client can show "date unknown"
// rather than presenting an import date as a capture date; the fallback exists
// so those files land in a sensible place instead of clumping at one end.
func (s *Store) TimelineNodes(ctx context.Context, ownerID uuid.UUID, from, to *time.Time, limit, offset int) ([]*Node, error) {
	limit = ClampSearchLimit(limit)
	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeCols+`
		`+nodeFrom+`
		JOIN media_meta mm ON mm.content_hash = coalesce(b.sha256, m.content_hash)
		WHERE n.owner_id = $1
		  AND n.trashed_at IS NULL
		  AND ($2::timestamptz IS NULL OR coalesce(mm.taken_at, n.updated_at) >= $2)
		  AND ($3::timestamptz IS NULL OR coalesce(mm.taken_at, n.updated_at) <= $3)
		ORDER BY coalesce(mm.taken_at, n.updated_at) DESC, n.id DESC
		LIMIT $4 OFFSET $5`,
		ownerID, from, to, limit, clampOffset(offset))
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// MediaMetaForNodes fetches metadata for many nodes at once.
//
// The timeline and an album page both render a grid of tiles, and every tile
// needs to know which variants exist before it can decide whether to request a
// thumbnail or fall back to the original. Asking per tile would be one query per
// photo on the hot path of the gallery — the N+1 that makes a 200-tile grid feel
// broken. Nodes with no analysed content are simply absent from the map.
//
// Filtered by VISIBILITY, not by ownership. This attaches metadata to nodes the
// caller has already been handed, so it widens nothing — but scoping it to
// owned nodes meant a shared album's tiles came back without dimensions or a
// variant list, and every one of them fell back to fetching the original.
func (s *Store) MediaMetaForNodes(ctx context.Context, userID uuid.UUID, nodeIDs []uuid.UUID) (map[uuid.UUID]*MediaMeta, error) {
	out := map[uuid.UUID]*MediaMeta{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT n.id, mm.width, mm.height, mm.orientation, mm.taken_at, mm.camera,
		       mm.gps_lat, mm.gps_lon, mm.duration_ms, mm.source,
		       coalesce(array_agg(mv.variant ORDER BY mv.variant)
		                FILTER (WHERE mv.variant IS NOT NULL), '{}')
		FROM nodes n
		LEFT JOIN file_versions v ON v.id = n.head_version_id
		LEFT JOIN blobs b         ON b.id = v.blob_id
		LEFT JOIN manifests man   ON man.id = v.manifest_id
		JOIN media_meta mm        ON mm.content_hash = coalesce(b.sha256, man.content_hash)
		LEFT JOIN media_variant mv ON mv.content_hash = mm.content_hash
		WHERE n.id = ANY($2) AND `+VisibleNodes+` AND n.trashed_at IS NULL
		GROUP BY n.id, mm.content_hash`, userID, nodeIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id            uuid.UUID
			m             MediaMeta
			width, height *int
			variants      []string
		)
		if err := rows.Scan(&id, &width, &height, &m.Orientation, &m.TakenAt, &m.Camera,
			&m.GPSLat, &m.GPSLon, &m.DurationMS, &m.Source, &variants); err != nil {
			return nil, err
		}
		if width != nil {
			m.Width = *width
		}
		if height != nil {
			m.Height = *height
		}
		m.Variants = variants
		out[id] = &m
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
