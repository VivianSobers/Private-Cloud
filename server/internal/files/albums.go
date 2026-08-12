package files

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Albums: user-ordered collections of nodes. See migration 00020 for why an
// album is a join table rather than a folder.
//
// Every query here is scoped by owner, and every mutation that names nodes
// re-derives them from the `nodes` table with an ownership filter rather than
// trusting the ids it was handed. An album is the one place in the system where
// a caller supplies a list of node ids out of nowhere — without that filter,
// adding a stranger's node id to your own album would make its metadata readable
// through the album's item list.

// MaxAlbumItemsPerRequest bounds one add or reorder call. A drag-reorder sends
// the whole order in one request by design, so this has to be generous; it
// exists to stop a single request pinning the database, not to limit album size.
const MaxAlbumItemsPerRequest = 5000

var (
	// ErrAlbumNotFound means no such album, or it is not this caller's.
	// Deliberately indistinguishable: telling someone an album exists but is not
	// theirs leaks the id space.
	ErrAlbumNotFound = errors.New("album not found")
	// ErrInvalidAlbumName is a blank or whitespace-only name.
	ErrInvalidAlbumName = errors.New("an album needs a name")
	// ErrTooManyAlbumItems means one request named more nodes than the cap.
	ErrTooManyAlbumItems = errors.New("too many items in one request")
)

// Album is a named, hand-ordered collection.
type Album struct {
	ID          uuid.UUID
	OwnerID     uuid.UUID
	Name        string
	Description string
	// CoverNodeID is the tile picture. Nil means "pick one" — the schema sets it
	// null rather than deleting the album when the cover photo goes away.
	CoverNodeID *uuid.UUID
	ItemCount   int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const albumCols = `
	a.id, a.owner_id, a.name, a.description, a.cover_node_id,
	a.created_at, a.updated_at,
	(SELECT count(*) FROM album_items ai WHERE ai.album_id = a.id)`

func scanAlbum(row pgx.Row) (*Album, error) {
	var a Album
	err := row.Scan(&a.ID, &a.OwnerID, &a.Name, &a.Description, &a.CoverNodeID,
		&a.CreatedAt, &a.UpdatedAt, &a.ItemCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAlbumNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListAlbums returns the owner's albums, most recently updated first.
func (s *Store) ListAlbums(ctx context.Context, ownerID uuid.UUID) ([]*Album, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+albumCols+`
		FROM albums a
		WHERE a.owner_id = $1
		ORDER BY a.updated_at DESC`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Album, 0, 16)
	for rows.Next() {
		a, err := scanAlbum(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAlbum returns one album, verifying ownership.
func (s *Store) GetAlbum(ctx context.Context, ownerID, albumID uuid.UUID) (*Album, error) {
	return scanAlbum(s.pool.QueryRow(ctx, `
		SELECT `+albumCols+`
		FROM albums a
		WHERE a.id = $1 AND a.owner_id = $2`, albumID, ownerID))
}

// CreateAlbum makes an empty album.
func (s *Store) CreateAlbum(ctx context.Context, ownerID uuid.UUID, name, description string) (*Album, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidAlbumName
	}
	return scanAlbum(s.pool.QueryRow(ctx, `
		WITH a AS (
			INSERT INTO albums (owner_id, name, description)
			VALUES ($1, $2, $3)
			RETURNING *
		)
		SELECT `+albumCols+` FROM a`, ownerID, name, description))
}

// AlbumPatch is the set of fields UpdateAlbum may change. A nil field is left
// alone; that is the difference between "do not touch the description" and "set
// it to empty", which one struct of plain strings cannot express.
type AlbumPatch struct {
	Name        *string
	Description *string
	// CoverNodeID set to a pointer-to-nil clears the cover; set to a node id
	// changes it; left nil leaves it as it was.
	CoverNodeID **uuid.UUID
}

// UpdateAlbum applies a partial change.
//
// A cover is verified to be one of the caller's own live files before it is
// stored. The column is only a foreign key to `nodes`, so without that check a
// caller could point their album's cover at someone else's photo and the tile
// would render it.
func (s *Store) UpdateAlbum(ctx context.Context, ownerID, albumID uuid.UUID, patch AlbumPatch) (*Album, error) {
	if patch.Name != nil {
		if strings.TrimSpace(*patch.Name) == "" {
			return nil, ErrInvalidAlbumName
		}
	}
	if patch.CoverNodeID != nil && *patch.CoverNodeID != nil {
		if err := s.assertOwnedLive(ctx, ownerID, **patch.CoverNodeID); err != nil {
			return nil, err
		}
	}

	// coalesce($n, column) expresses "leave it alone" for name and description.
	// The cover cannot use that trick, because NULL is a meaningful value for it
	// — clearing a cover and not touching one are different requests — so it
	// carries its own boolean.
	var (
		name        any
		description any
		setCover    = patch.CoverNodeID != nil
		cover       *uuid.UUID
	)
	if patch.Name != nil {
		name = strings.TrimSpace(*patch.Name)
	}
	if patch.Description != nil {
		description = *patch.Description
	}
	if setCover {
		cover = *patch.CoverNodeID
	}

	return scanAlbum(s.pool.QueryRow(ctx, `
		WITH a AS (
			UPDATE albums
			SET name = coalesce($3, name),
			    description = coalesce($4, description),
			    cover_node_id = CASE WHEN $5 THEN $6 ELSE cover_node_id END,
			    updated_at = now()
			WHERE id = $1 AND owner_id = $2
			RETURNING *
		)
		SELECT `+albumCols+` FROM a`,
		albumID, ownerID, name, description, setCover, cover))
}

// DeleteAlbum removes the album and its membership rows. It never touches file
// content — album_items cascades, nodes are untouched. This is the question
// every user has before they click it, so it is worth being explicit.
func (s *Store) DeleteAlbum(ctx context.Context, ownerID, albumID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM albums WHERE id = $1 AND owner_id = $2`, albumID, ownerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAlbumNotFound
	}
	return nil
}

// AlbumItems returns an album's nodes in the user's order.
//
// The owner filter is applied to the NODES as well as to the album. Membership
// rows are not themselves an authorisation: this is the read that would expose a
// foreign node if one ever got into the join table.
func (s *Store) AlbumItems(ctx context.Context, ownerID, albumID uuid.UUID, limit, offset int) ([]*Node, error) {
	if _, err := s.GetAlbum(ctx, ownerID, albumID); err != nil {
		return nil, err
	}
	limit = ClampSearchLimit(limit)
	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeCols+`
		`+nodeFrom+`
		JOIN album_items ai ON ai.node_id = n.id
		WHERE ai.album_id = $1 AND n.owner_id = $2 AND n.trashed_at IS NULL
		ORDER BY ai.position, ai.added_at, n.id
		LIMIT $3 OFFSET $4`,
		albumID, ownerID, limit, clampOffset(offset))
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// AddAlbumItems appends nodes to an album, preserving the order they were given
// in and skipping any that are not the caller's own live files.
//
// Adding a node already in the album is a no-op rather than an error or a
// duplicate tile — the primary key does that — which is what makes a retried
// request safe. Returns how many rows were actually added.
func (s *Store) AddAlbumItems(ctx context.Context, ownerID, albumID uuid.UUID, nodeIDs []uuid.UUID) (int64, error) {
	if len(nodeIDs) > MaxAlbumItemsPerRequest {
		return 0, ErrTooManyAlbumItems
	}
	if _, err := s.GetAlbum(ctx, ownerID, albumID); err != nil {
		return 0, err
	}
	if len(nodeIDs) == 0 {
		return 0, nil
	}

	// WITH ORDINALITY is what preserves the caller's ordering: unnest alone has
	// no defined order, so a multi-select drag would land in an arbitrary one.
	tag, err := s.pool.Exec(ctx, `
		WITH base AS (
			SELECT coalesce(max(position), 0) AS p FROM album_items WHERE album_id = $1
		), incoming AS (
			SELECT u.id, u.ord FROM unnest($3::uuid[]) WITH ORDINALITY AS u(id, ord)
		)
		INSERT INTO album_items (album_id, node_id, position)
		SELECT $1, n.id, base.p + incoming.ord
		FROM incoming
		JOIN nodes n ON n.id = incoming.id
		            AND n.owner_id = $2
		            AND n.trashed_at IS NULL
		            AND n.kind = 'file'
		CROSS JOIN base
		ON CONFLICT (album_id, node_id) DO NOTHING`,
		albumID, ownerID, nodeIDs)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() > 0 {
		if err := s.touchAlbum(ctx, ownerID, albumID); err != nil {
			return tag.RowsAffected(), err
		}
	}
	return tag.RowsAffected(), nil
}

// RemoveAlbumItem takes one node out of an album. The file itself is untouched.
func (s *Store) RemoveAlbumItem(ctx context.Context, ownerID, albumID, nodeID uuid.UUID) error {
	if _, err := s.GetAlbum(ctx, ownerID, albumID); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM album_items WHERE album_id = $1 AND node_id = $2`, albumID, nodeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return s.touchAlbum(ctx, ownerID, albumID)
}

// ReorderAlbum replaces the whole order in one statement.
//
// Whole-order rather than per-item position updates: a drag-reorder that issued
// N updates would be N chances to end up half-applied, and the intermediate
// states are not orders anybody asked for. Ids that are not in the album are
// ignored rather than rejected, so a client racing a concurrent removal still
// gets a consistent result instead of an error it cannot act on.
func (s *Store) ReorderAlbum(ctx context.Context, ownerID, albumID uuid.UUID, nodeIDs []uuid.UUID) error {
	if len(nodeIDs) > MaxAlbumItemsPerRequest {
		return ErrTooManyAlbumItems
	}
	if _, err := s.GetAlbum(ctx, ownerID, albumID); err != nil {
		return err
	}
	if len(nodeIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE album_items ai
		SET position = u.ord
		FROM unnest($2::uuid[]) WITH ORDINALITY AS u(id, ord)
		WHERE ai.album_id = $1 AND ai.node_id = u.id`, albumID, nodeIDs)
	if err != nil {
		return err
	}
	return s.touchAlbum(ctx, ownerID, albumID)
}

// touchAlbum bumps updated_at so the album list re-sorts after a membership
// change. Membership lives in another table, so nothing else would move it.
func (s *Store) touchAlbum(ctx context.Context, ownerID, albumID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE albums SET updated_at = now() WHERE id = $1 AND owner_id = $2`, albumID, ownerID)
	return err
}
