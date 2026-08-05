package files

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Version is one immutable snapshot of a file's content.
//
// Phase 1 wrote a row per overwrite and kept none but the head; the table always
// held the history, unexposed. This is the shape that finally surfaces it. The
// content hash is coalesced across the two storage formats exactly as a live
// node's is, so a client can verify a specific version's download.
type Version struct {
	ID          uuid.UUID
	Size        int64
	MIME        string
	CreatedAt   time.Time
	CreatedBy   *uuid.UUID
	IsHead      bool
	ContentHash []byte
	ManifestID  *uuid.UUID
}

// ListVersions returns a file's versions, newest first, head flagged.
//
// Ownership and liveness are in the WHERE clause, not an assertion afterwards:
// the join to nodes is what authorises the read, so no caller can turn a node id
// into a cross-tenant history dump. A file always has at least one version, so
// an empty result means the node does not exist, is not this user's, or is in
// the trash — reported as ErrNotFound rather than an empty list that a client
// would misread as "a file with no history".
func (s *Store) ListVersions(ctx context.Context, ownerID, nodeID uuid.UUID) ([]Version, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.id, v.size, v.mime, v.created_at, v.created_by,
		       (v.id = n.head_version_id) AS is_head,
		       coalesce(b.sha256, m.content_hash), v.manifest_id
		FROM file_versions v
		JOIN nodes n           ON n.id = v.node_id
		LEFT JOIN blobs b      ON b.id = v.blob_id
		LEFT JOIN manifests m  ON m.id = v.manifest_id
		WHERE v.node_id = $1 AND n.owner_id = $2 AND n.trashed_at IS NULL
		ORDER BY v.created_at DESC, v.id DESC`, nodeID, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.Size, &v.MIME, &v.CreatedAt, &v.CreatedBy,
			&v.IsHead, &v.ContentHash, &v.ManifestID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// RestoreVersion makes an old version current by adding a NEW version that
// points at the same content — never by deleting the versions in between.
//
// Undoing a mistake by destroying the history after it is how one mistake
// becomes two: the versions a user rolled past are exactly the ones they may
// want back. So this is an append, and it reuses the target's blob or manifest
// rather than copying bytes — the refcount trigger credits the shared content on
// INSERT, and a restore of a 4 GB file costs one row.
func (s *Store) RestoreVersion(ctx context.Context, ownerID, nodeID, versionID uuid.UUID) (*Node, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// The node must be ours, live and a file. Ownership here is the authorisation
	// for the whole operation.
	var kind string
	var headSize int64
	err = tx.QueryRow(ctx, `
		SELECT n.kind, coalesce(v.size, 0)
		FROM nodes n
		LEFT JOIN file_versions v ON v.id = n.head_version_id
		WHERE n.id = $1 AND n.owner_id = $2 AND n.trashed_at IS NULL`,
		nodeID, ownerID).Scan(&kind, &headSize)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if kind != KindFile {
		return nil, ErrNotAFile
	}

	// The target version, scoped to this node so a version id from another file
	// cannot be grafted on. Reusing its content pointers is what makes restore
	// free.
	var (
		blobID, manifestID *uuid.UUID
		size               int64
		mime               string
	)
	err = tx.QueryRow(ctx, `
		SELECT blob_id, manifest_id, size, mime
		FROM file_versions WHERE id = $1 AND node_id = $2`,
		versionID, nodeID).Scan(&blobID, &manifestID, &size, &mime)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	// Only the growth is charged: quota counts the head version, and restore
	// swaps one head size for another. A restore that shrinks the file, or leaves
	// it the same, never fails on quota.
	if size > headSize {
		if err := checkQuota(ctx, tx, ownerID, size-headSize); err != nil {
			return nil, err
		}
	}

	var newVersionID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO file_versions (node_id, blob_id, manifest_id, size, mime, created_by)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		nodeID, blobID, manifestID, size, mime, ownerID).Scan(&newVersionID); err != nil {
		return nil, fmt.Errorf("create restore version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE nodes SET head_version_id = $2, updated_at = now() WHERE id = $1`,
		nodeID, newVersionID); err != nil {
		return nil, fmt.Errorf("set head version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, ownerID, nodeID)
}
