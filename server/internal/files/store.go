package files

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the persistence layer for the tree.
//
// Queries stay hand-written, matching auth.Store. The count is now around
// thirty; sqlc starts to look worthwhile somewhere past that, and Phase 2's
// chunk manifests are the natural moment to introduce it.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// nodeCols joins through to the head version so a listing does not need a
// second query per file to learn its size — the N+1 that makes folder listings
// feel slow once a directory has a few hundred entries.
// coalesce over the two storage formats: a version points at a blob OR a
// manifest (schema CHECK), so exactly one hash column is non-NULL.
const nodeCols = `
	n.id, n.owner_id, n.parent_id, n.kind, n.name, n.path,
	n.head_version_id, n.trashed_at, n.trashed_root_id, n.created_at, n.updated_at,
	v.size, v.mime, coalesce(b.sha256, m.content_hash), b.storage_key, v.manifest_id`

const nodeFrom = `
	FROM nodes n
	LEFT JOIN file_versions v ON v.id = n.head_version_id
	LEFT JOIN blobs b         ON b.id = v.blob_id
	LEFT JOIN manifests m     ON m.id = v.manifest_id`

func scanNode(row pgx.Row) (*Node, error) {
	var (
		n    Node
		size *int64
		mime *string
		hash []byte
		key  *string
	)
	err := row.Scan(
		&n.ID, &n.OwnerID, &n.ParentID, &n.Kind, &n.Name, &n.Path,
		&n.HeadVersionID, &n.TrashedAt, &n.TrashedRootID, &n.CreatedAt, &n.UpdatedAt,
		&size, &mime, &hash, &key, &n.ManifestID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if size != nil {
		n.Size = *size
	}
	if mime != nil {
		n.MIME = *mime
	}
	n.ContentHash = hash
	if key != nil {
		n.BlobKey = *key
	}
	return &n, nil
}

func scanNodes(rows pgx.Rows) ([]*Node, error) {
	defer rows.Close()
	out := make([]*Node, 0, 16)
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// --- reads ------------------------------------------------------------------

// EnsureRoot returns the user's root folder, creating it on first use.
//
// Created lazily rather than as part of user creation so that `cloudctl user
// create`, the recovery path, and any future provisioning route all converge on
// the same code — a root that only exists if one particular code path ran is a
// class of bug that only shows up for the accounts made the other way.
func (s *Store) EnsureRoot(ctx context.Context, ownerID uuid.UUID) (*Node, error) {
	if n, err := s.root(ctx, ownerID); err == nil {
		return n, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO nodes (owner_id, parent_id, kind, name, name_fold, path)
		VALUES ($1, NULL, 'folder', '', '', '/')`, ownerID)
	// A concurrent request may have won the race; the partial unique index on
	// (owner_id) WHERE parent_id IS NULL is what makes that safe to ignore.
	if err != nil && !uniqueViolation(err) {
		return nil, fmt.Errorf("create root: %w", err)
	}
	return s.root(ctx, ownerID)
}

func (s *Store) root(ctx context.Context, ownerID uuid.UUID) (*Node, error) {
	return scanNode(s.pool.QueryRow(ctx, `SELECT `+nodeCols+nodeFrom+`
		WHERE n.owner_id = $1 AND n.parent_id IS NULL AND n.trashed_at IS NULL`, ownerID))
}

// Get fetches one node. ownerID is part of the WHERE clause, not an assertion
// after the fact: authorisation belongs in the query, so no handler can forget
// it and turn an id into a cross-tenant read.
func (s *Store) Get(ctx context.Context, ownerID, id uuid.UUID) (*Node, error) {
	return scanNode(s.pool.QueryRow(ctx, `SELECT `+nodeCols+nodeFrom+`
		WHERE n.id = $1 AND n.owner_id = $2`, id, ownerID))
}

// GetLive is Get, restricted to nodes not in the trash.
func (s *Store) GetLive(ctx context.Context, ownerID, id uuid.UUID) (*Node, error) {
	return scanNode(s.pool.QueryRow(ctx, `SELECT `+nodeCols+nodeFrom+`
		WHERE n.id = $1 AND n.owner_id = $2 AND n.trashed_at IS NULL`, id, ownerID))
}

// GetByPath resolves an absolute path. WebDAV in slice 6 addresses everything
// this way, which is the reason path is materialised at all.
func (s *Store) GetByPath(ctx context.Context, ownerID uuid.UUID, path string) (*Node, error) {
	return scanNode(s.pool.QueryRow(ctx, `SELECT `+nodeCols+nodeFrom+`
		WHERE n.owner_id = $1 AND n.path = $2 AND n.trashed_at IS NULL`, ownerID, path))
}

// ListChildren returns the live contents of a folder, folders first then files,
// each alphabetically by folded name — the ordering every file manager uses.
func (s *Store) ListChildren(ctx context.Context, ownerID, parentID uuid.UUID) ([]*Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nodeCols+nodeFrom+`
		WHERE n.owner_id = $1 AND n.parent_id = $2 AND n.trashed_at IS NULL
		ORDER BY (n.kind = 'file'), n.name_fold`, ownerID, parentID)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// ListTrash returns only the nodes the user actually deleted, not everything
// that came along inside them. Showing all 4,000 rows of a deleted folder would
// make the trash unusable.
func (s *Store) ListTrash(ctx context.Context, ownerID uuid.UUID) ([]*Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nodeCols+nodeFrom+`
		WHERE n.owner_id = $1 AND n.trashed_root_id = n.id
		ORDER BY n.trashed_at DESC`, ownerID)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// usageBytes is the accounting every "how much is this owner using" question
// answers from, so a quota refusal and the number shown next to it can never
// disagree.
//
// Two properties it has to have, and each was got wrong by an earlier form:
//
//   - It counts SUPERSEDED VERSIONS, not only head versions. With KeepVersions
//     at 25 and a 90-day retention, a genuinely-changing file kept two dozen
//     older copies that cost real disk and were charged to nobody.
//   - It counts each distinct CONTENT once. That is what makes the above
//     survivable: with content addressing, a file saved fifty times without
//     changing is one copy on disk, so charging fifty would invent usage that
//     does not exist. DISTINCT ON over the content hash is the same identity
//     dedup itself uses.
//
// The ORDER BY picks which bucket shared content lands in: a live head first,
// then any head, then whatever is left. So a file that is both current and
// present in an older version is reported as live, once — never split across
// two buckets and never counted twice.
const usageBytes = `
	WITH content AS (
		SELECT DISTINCT ON (coalesce(b.sha256, m.content_hash))
		       v.size                                AS size,
		       v.id = n.head_version_id              AS is_head,
		       n.trashed_at IS NOT NULL              AS trashed
		FROM nodes n
		JOIN file_versions v  ON v.node_id = n.id
		LEFT JOIN blobs b     ON b.id = v.blob_id
		LEFT JOIN manifests m ON m.id = v.manifest_id
		WHERE n.owner_id = $1 AND n.kind = 'file'
		ORDER BY coalesce(b.sha256, m.content_hash),
		         (v.id = n.head_version_id AND n.trashed_at IS NULL) DESC,
		         (v.id = n.head_version_id) DESC
	)
	SELECT
		coalesce(sum(size) FILTER (WHERE is_head AND NOT trashed), 0),
		coalesce(sum(size) FILTER (WHERE is_head AND trashed), 0),
		coalesce(sum(size) FILTER (WHERE NOT is_head), 0)
	FROM content`

func (s *Store) Usage(ctx context.Context, ownerID uuid.UUID) (Usage, error) {
	var u Usage
	if err := s.pool.QueryRow(ctx, usageBytes, ownerID).
		Scan(&u.LiveBytes, &u.TrashBytes, &u.VersionBytes); err != nil {
		return u, err
	}
	// File count is nodes, not content: two identical files are two files to the
	// person looking at them, however few bytes they cost.
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM nodes
		WHERE owner_id = $1 AND kind = 'file' AND trashed_at IS NULL`, ownerID).
		Scan(&u.FileCount); err != nil {
		return u, err
	}
	err := s.pool.QueryRow(ctx, `SELECT quota_bytes FROM users WHERE id = $1`, ownerID).Scan(&u.QuotaBytes)
	return u, err
}

// --- writes -----------------------------------------------------------------

// CreateFolder adds a folder under parentID.
func (s *Store) CreateFolder(ctx context.Context, ownerID, parentID uuid.UUID, name string) (*Node, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	parent, err := s.GetLive(ctx, ownerID, parentID)
	if err != nil {
		return nil, err
	}
	if !parent.IsFolder() {
		return nil, ErrNotAFolder
	}

	var id uuid.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO nodes (owner_id, parent_id, kind, name, name_fold, path)
		VALUES ($1, $2, 'folder', $3, $4, $5)
		RETURNING id`,
		ownerID, parentID, name, Fold(name), JoinPath(parent.Path, name),
	).Scan(&id)

	if uniqueViolation(err) {
		return nil, ErrNameTaken
	}
	if err != nil {
		return nil, fmt.Errorf("create folder: %w", err)
	}
	return s.Get(ctx, ownerID, id)
}

// PutFileInput carries the result of a completed content write, in exactly one
// of the two storage formats: a whole-file blob (BlobKey + SHA256) or a CAS
// manifest (ManifestID). The schema enforces the same exclusivity with a CHECK
// constraint; validating it here too turns a wiring mistake into an error at
// the call site instead of a constraint violation two layers down.
type PutFileInput struct {
	OwnerID  uuid.UUID
	ParentID uuid.UUID
	Name     string

	BlobKey string
	SHA256  []byte

	ManifestID uuid.UUID

	Size int64
	MIME string
}

// PutFile records an already-written blob as a file.
//
// The bytes are on disk before this is called — that ordering is the whole
// crash-safety story. If this transaction fails or the process dies here, the
// blob is unreferenced and fsck reclaims it. The opposite order would produce a
// row that promises content nobody can serve.
//
// If a live file of the same name exists, this adds a version and moves the
// head rather than erroring: uploading over an existing file is what every
// client expects, and refusing it would make WebDAV's PUT semantics impossible.
func (s *Store) PutFile(ctx context.Context, in PutFileInput) (*Node, error) {
	if err := ValidateName(in.Name); err != nil {
		return nil, err
	}
	if (in.BlobKey == "") == (in.ManifestID == uuid.Nil) {
		return nil, fmt.Errorf("PutFile requires exactly one of BlobKey or ManifestID")
	}
	if in.MIME == "" {
		in.MIME = "application/octet-stream"
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Parent must exist, be ours, be live and be a folder.
	var parentPath, parentKind string
	err = tx.QueryRow(ctx, `
		SELECT path, kind FROM nodes
		WHERE id = $1 AND owner_id = $2 AND trashed_at IS NULL`,
		in.ParentID, in.OwnerID).Scan(&parentPath, &parentKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if parentKind != KindFolder {
		return nil, ErrNotAFolder
	}

	// Quota is checked inside the transaction against the same snapshot the
	// insert will use. Two simultaneous uploads can still both pass and land
	// slightly over — accepted deliberately, because the alternative is
	// serialising every upload behind a lock on the user row to defend a limit
	// that is advisory anyway.
	if err := checkQuota(ctx, tx, in.OwnerID, in.Size); err != nil {
		return nil, err
	}

	// The blob row exists only for whole-file storage. A manifest-backed version
	// references the manifest committed by cas.Write — the chunk rows and their
	// bytes are already durable by the time this transaction starts, the same
	// bytes-before-row ordering the blob path relies on.
	var blobID *uuid.UUID
	if in.BlobKey != "" {
		blobID = new(uuid.UUID)
		if err := tx.QueryRow(ctx, `
			INSERT INTO blobs (storage_key, size, sha256) VALUES ($1,$2,$3) RETURNING id`,
			in.BlobKey, in.Size, in.SHA256).Scan(blobID); err != nil {
			return nil, fmt.Errorf("record blob: %w", err)
		}
	}
	var manifestID *uuid.UUID
	if in.ManifestID != uuid.Nil {
		manifestID = &in.ManifestID
	}

	// Is there already a live sibling with this name?
	var (
		nodeID   uuid.UUID
		nodeKind string
	)
	err = tx.QueryRow(ctx, `
		SELECT id, kind FROM nodes
		WHERE parent_id = $1 AND name_fold = $2 AND trashed_at IS NULL`,
		in.ParentID, Fold(in.Name)).Scan(&nodeID, &nodeKind)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx, `
			INSERT INTO nodes (owner_id, parent_id, kind, name, name_fold, path)
			VALUES ($1,$2,'file',$3,$4,$5) RETURNING id`,
			in.OwnerID, in.ParentID, in.Name, Fold(in.Name), JoinPath(parentPath, in.Name),
		).Scan(&nodeID)
		if uniqueViolation(err) {
			// Lost a race with a concurrent upload of the same name.
			return nil, ErrNameTaken
		}
		if err != nil {
			return nil, fmt.Errorf("create file node: %w", err)
		}
	case err != nil:
		return nil, err
	case nodeKind != KindFile:
		// A folder is sitting where the file would go. Silently replacing it
		// would delete a subtree the user never asked to delete.
		return nil, ErrNameTaken
	}

	var versionID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO file_versions (node_id, blob_id, manifest_id, size, mime, created_by)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		nodeID, blobID, manifestID, in.Size, in.MIME, in.OwnerID).Scan(&versionID); err != nil {
		return nil, fmt.Errorf("create version: %w", err)
	}

	// Preserve the display casing of the new upload ("README.md" replacing
	// "readme.md" should show the name the user just sent), while leaving the
	// folded name — and therefore identity — unchanged.
	if _, err := tx.Exec(ctx, `
		UPDATE nodes SET head_version_id = $2, name = $3, updated_at = now() WHERE id = $1`,
		nodeID, versionID, in.Name); err != nil {
		return nil, fmt.Errorf("set head version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, in.OwnerID, nodeID)
}

func checkQuota(ctx context.Context, tx pgx.Tx, ownerID uuid.UUID, adding int64) error {
	var quota *int64
	if err := tx.QueryRow(ctx, `SELECT quota_bytes FROM users WHERE id = $1`, ownerID).Scan(&quota); err != nil {
		return err
	}
	if quota == nil {
		return nil // unlimited
	}

	// The SAME accounting the usage endpoint reports, down to the query text.
	// Two definitions of "used" is how a user gets refused at 4 GiB while their
	// storage page shows 2, with nothing on either screen to explain it.
	//
	// Trashed files count, because they still occupy the disk and a quota that
	// ignores them lets someone store twice their allowance by never emptying the
	// trash. Retained older versions count for exactly the same reason.
	var live, trashed, versions int64
	if err := tx.QueryRow(ctx, usageBytes, ownerID).Scan(&live, &trashed, &versions); err != nil {
		return err
	}
	if live+trashed+versions+adding > *quota {
		return ErrQuota
	}
	return nil
}

// MoveOrRename changes a node's name, its parent, or both.
//
// newParentID may be uuid.Nil to keep the current parent; newName may be empty
// to keep the current name. Both halves happen in one transaction because the
// materialised paths of every descendant have to move with the node — a partial
// application would leave children whose path no longer matches their parent
// chain, and every prefix query would then return the wrong subtree.
func (s *Store) MoveOrRename(ctx context.Context, ownerID, id uuid.UUID, newParentID uuid.UUID, newName string) (*Node, error) {
	node, err := s.GetLive(ctx, ownerID, id)
	if err != nil {
		return nil, err
	}
	if node.IsRoot() {
		return nil, ErrRootReserved
	}

	name := node.Name
	if newName != "" {
		if err := ValidateName(newName); err != nil {
			return nil, err
		}
		name = newName
	}

	parentID := *node.ParentID
	if newParentID != uuid.Nil {
		parentID = newParentID
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var parentPath, parentKind string
	err = tx.QueryRow(ctx, `
		SELECT path, kind FROM nodes
		WHERE id = $1 AND owner_id = $2 AND trashed_at IS NULL`,
		parentID, ownerID).Scan(&parentPath, &parentKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if parentKind != KindFolder {
		return nil, ErrNotAFolder
	}

	// Moving a folder into its own subtree would detach that subtree from the
	// root entirely: the rows survive, but nothing reaches them and the paths
	// become self-referential. The prefix test catches it before that happens.
	if node.IsFolder() && (parentID == node.ID || strings.HasPrefix(parentPath+"/", node.Path+"/")) {
		return nil, ErrCycle
	}

	newPath := JoinPath(parentPath, name)
	if newPath == node.Path && parentID == *node.ParentID {
		return node, nil // no-op
	}

	_, err = tx.Exec(ctx, `
		UPDATE nodes SET parent_id = $2, name = $3, name_fold = $4, path = $5, updated_at = now()
		WHERE id = $1`, id, parentID, name, Fold(name), newPath)
	if uniqueViolation(err) {
		return nil, ErrNameTaken
	}
	if err != nil {
		return nil, fmt.Errorf("move node: %w", err)
	}

	// Rewrite descendants. Splicing on the old prefix length rather than
	// recomputing each path from its parent keeps this one statement instead of
	// a recursive walk.
	if node.IsFolder() {
		// $4 is cast explicitly: substring(text from int) leaves the parameter's
		// type ambiguous, and pgx then has to guess an encoding for it. Without
		// the cast this fails at execution with "cannot find encode plan".
		if _, err := tx.Exec(ctx, `
			UPDATE nodes
			SET path = $3 || substring(path from $4::int), updated_at = now()
			WHERE owner_id = $1 AND path LIKE $2`,
			ownerID, likePrefix(node.Path)+"/%", newPath, len(node.Path)+1); err != nil {
			return nil, fmt.Errorf("rewrite descendant paths: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, ownerID, id)
}

// Trash soft-deletes a node and everything beneath it.
func (s *Store) Trash(ctx context.Context, ownerID, id uuid.UUID) (int64, error) {
	node, err := s.GetLive(ctx, ownerID, id)
	if err != nil {
		return 0, err
	}
	if node.IsRoot() {
		return 0, ErrRootReserved
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// One timestamp for the whole subtree, taken once, so the rows genuinely
	// share an identity as a single delete rather than merely landing close
	// together in time.
	now := time.Now()

	tag, err := tx.Exec(ctx, `
		UPDATE nodes SET trashed_at = $3, trashed_root_id = $2, updated_at = now()
		WHERE id = $2 AND owner_id = $1 AND trashed_at IS NULL`, ownerID, id, now)
	if err != nil {
		return 0, err
	}
	affected := tag.RowsAffected()
	if affected == 0 {
		return 0, ErrNotFound
	}

	if node.IsFolder() {
		tag, err = tx.Exec(ctx, `
			UPDATE nodes SET trashed_at = $4, trashed_root_id = $3, updated_at = now()
			WHERE owner_id = $1 AND path LIKE $2 AND trashed_at IS NULL`,
			ownerID, likePrefix(node.Path)+"/%", id, now)
		if err != nil {
			return 0, err
		}
		affected += tag.RowsAffected()
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return affected, nil
}

// Restore undoes a Trash. Only the node the user actually deleted can be
// restored; restoring an arbitrary row from inside a deleted folder would put a
// live node under a trashed parent, which no other query is prepared for.
func (s *Store) Restore(ctx context.Context, ownerID, id uuid.UUID) (*Node, error) {
	node, err := s.Get(ctx, ownerID, id)
	if err != nil {
		return nil, err
	}
	if !node.IsTrashed() {
		return nil, ErrNotTrashed
	}
	if node.TrashedRootID == nil || *node.TrashedRootID != node.ID {
		return nil, fmt.Errorf("%w: restore the folder it was deleted with instead", ErrNotTrashed)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// The parent may have been trashed since. Restoring into it would recreate
	// exactly the inconsistency the trash-root rule exists to prevent.
	var parentTrashed *time.Time
	err = tx.QueryRow(ctx, `SELECT trashed_at FROM nodes WHERE id = $1 AND owner_id = $2`,
		node.ParentID, ownerID).Scan(&parentTrashed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: the folder it lived in no longer exists", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if parentTrashed != nil {
		return nil, fmt.Errorf("%w: restore its parent folder first", ErrNotTrashed)
	}

	_, err = tx.Exec(ctx, `
		UPDATE nodes SET trashed_at = NULL, trashed_root_id = NULL, updated_at = now()
		WHERE trashed_root_id = $1 AND owner_id = $2`, id, ownerID)
	if uniqueViolation(err) {
		// Something else took the name while this was in the trash. Renaming on
		// the user's behalf would be a surprise; making them choose is honest.
		return nil, ErrNameTaken
	}
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, ownerID, id)
}

// Purge permanently removes a trashed node and its subtree. The rows go via
// ON DELETE CASCADE; the blobs are left for GC, which is what makes purge fast
// and interruptible — the bytes are reclaimed by a separate, resumable pass.
func (s *Store) Purge(ctx context.Context, ownerID, id uuid.UUID) error {
	node, err := s.Get(ctx, ownerID, id)
	if err != nil {
		return err
	}
	if !node.IsTrashed() {
		return ErrNotTrashed
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM nodes WHERE id = $1 AND owner_id = $2`, id, ownerID)
	return err
}

// EmptyTrash purges every trash root for a user.
func (s *Store) EmptyTrash(ctx context.Context, ownerID uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM nodes WHERE owner_id = $1 AND trashed_root_id = id`, ownerID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// AutoPurgeTrash removes trashed nodes older than the retention window.
func (s *Store) AutoPurgeTrash(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM nodes
		WHERE trashed_root_id = id AND trashed_at < now() - $1::interval`, olderThan.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// --- blob lifecycle ---------------------------------------------------------

// BlobRef identifies a stored blob for the garbage collector.
type BlobRef struct {
	ID         uuid.UUID
	StorageKey string
	Size       int64
	SHA256     []byte
}

// UnreferencedBlobs lists blobs no version points at any more.
//
// The grace period is not cosmetic. A blob row is created before the version
// that references it, so an upload in flight is legitimately at refcount zero
// for a few milliseconds — and a GC pass that ignored that would delete the
// bytes of a file being written right now.
func (s *Store) UnreferencedBlobs(ctx context.Context, grace time.Duration, limit int) ([]BlobRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_key, size, sha256 FROM blobs
		WHERE refcount = 0 AND created_at < now() - $1::interval
		ORDER BY created_at
		LIMIT $2`, grace.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BlobRef
	for rows.Next() {
		var b BlobRef
		if err := rows.Scan(&b.ID, &b.StorageKey, &b.Size, &b.SHA256); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBlobRow removes the row, but only if it is still unreferenced. The
// refcount condition closes the window between UnreferencedBlobs listing a blob
// and the GC deleting it, in which a new version could have claimed it.
func (s *Store) DeleteBlobRow(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM blobs WHERE id = $1 AND refcount = 0`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// AllBlobKeys returns every storage key the database knows about, for fsck's
// orphan detection.
func (s *Store) AllBlobKeys(ctx context.Context) (map[string]BlobRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, storage_key, size, sha256 FROM blobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]BlobRef)
	for rows.Next() {
		var b BlobRef
		if err := rows.Scan(&b.ID, &b.StorageKey, &b.Size, &b.SHA256); err != nil {
			return nil, err
		}
		out[b.StorageKey] = b
	}
	return out, rows.Err()
}

// --- blob → manifest migration ----------------------------------------------

// MigratableVersion is a whole-file version large enough to be worth chunking.
type MigratableVersion struct {
	VersionID  uuid.UUID
	BlobID     uuid.UUID
	StorageKey string
	Size       int64
}

// MigratableVersions lists blob-backed file versions eligible for background
// migration to content-addressed storage.
//
// minSize excludes the files that must stay whole-file blobs permanently: below
// the chunker's threshold a manifest row plus a chunk row to describe a few
// hundred bytes is pure overhead, so migrating them would cost more than it
// saves and never dedup against anything. blob_id NOT NULL is implied — the
// storage CHECK makes manifest_id IS NULL equivalent to blob-backed — and the
// JOIN drops anything already migrated.
func (s *Store) MigratableVersions(ctx context.Context, minSize int64, limit int) ([]MigratableVersion, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.id, v.blob_id, b.storage_key, v.size
		FROM file_versions v
		JOIN blobs b ON b.id = v.blob_id
		WHERE v.manifest_id IS NULL AND v.size >= $1
		ORDER BY v.created_at
		LIMIT $2`, minSize, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MigratableVersion
	for rows.Next() {
		var v MigratableVersion
		if err := rows.Scan(&v.VersionID, &v.BlobID, &v.StorageKey, &v.Size); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SwitchToManifest repoints a version from its blob to a manifest, in place.
//
// The guard is the whole correctness story. `blob_id = $2 AND manifest_id IS
// NULL` means the switch applies only if the version STILL points at the exact
// blob we chunked and nothing else has migrated it — so a concurrent pass, an
// overwrite that replaced the head, or a purge that deleted the row all land as
// a no-op (rows affected 0) rather than clobbering a newer state. The AFTER
// UPDATE trigger drops the old blob's refcount as the reference moves off it;
// GC reclaims the bytes on a later pass, the same single reclaimer every path
// defers to. Returns false when the version changed underneath, leaving the
// freshly written manifest orphaned for the caller to drop.
func (s *Store) SwitchToManifest(ctx context.Context, versionID, blobID, manifestID uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE file_versions
		SET blob_id = NULL, manifest_id = $3
		WHERE id = $1 AND blob_id = $2 AND manifest_id IS NULL`,
		versionID, blobID, manifestID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// --- helpers ----------------------------------------------------------------

// likePrefix escapes the LIKE metacharacters so a folder genuinely named
// "100%_done" matches only itself. Without this, one badly named folder turns
// a subtree update into a wildcard that rewrites unrelated paths.
func likePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p)
}
