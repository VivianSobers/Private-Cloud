package files

import (
	"context"
	"time"

	"github.com/google/uuid"
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
