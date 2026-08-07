package files

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// Tags on files: cheap, explainable, reversible. Auto tags are re-derived on each
// extraction and replace only prior auto tags; user tags are explicit and never
// touched by re-tagging. See migration 00014 for the shape.

// Tag is a label on a node and how it got there.
type Tag struct {
	Name   string
	Source string // "auto" or "user"
}

// TagCount is a tag and how many of a user's live files carry it.
type TagCount struct {
	Tag   string
	Count int64
}

const maxTagLen = 64

// normalizeTag lowercases and trims a tag to a stable form, so "Invoice" and
// "invoice " are the same tag rather than two.
func normalizeTag(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}

// hasControlChar reports whether a string contains an ASCII control character,
// which has no place in a tag and is a classic injection vector on display.
func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// SetAutoTags replaces a node's automatic tags, leaving user tags untouched. The
// worker calls this by node id (it is trusted and already scoped the job to an
// owner); user-facing mutations below verify ownership.
func (s *Store) SetAutoTags(ctx context.Context, nodeID uuid.UUID, tags []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx,
		`DELETE FROM node_tags WHERE node_id = $1 AND source = 'auto'`, nodeID); err != nil {
		return err
	}
	for _, raw := range tags {
		tag := normalizeTag(raw)
		if tag == "" || len(tag) > maxTagLen {
			continue
		}
		// DO NOTHING on conflict: a tag the user already applied stays a user tag
		// rather than being duplicated or downgraded to auto.
		if _, err := tx.Exec(ctx, `
			INSERT INTO node_tags (node_id, tag, source) VALUES ($1, $2, 'auto')
			ON CONFLICT (node_id, tag) DO NOTHING`, nodeID, tag); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AddUserTag adds an explicit tag to one of the owner's files. If the tag already
// exists as an auto tag it is promoted to a user tag, so re-tagging will not
// remove it.
func (s *Store) AddUserTag(ctx context.Context, ownerID, nodeID uuid.UUID, raw string) error {
	tag := normalizeTag(raw)
	if tag == "" || len(tag) > maxTagLen || hasControlChar(tag) {
		// A control character in a tag is a log- or display-injection vector once
		// the tag is echoed back; reject it at the door.
		return ErrInvalidName
	}
	if err := s.assertOwnedLive(ctx, ownerID, nodeID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO node_tags (node_id, tag, source) VALUES ($1, $2, 'user')
		ON CONFLICT (node_id, tag) DO UPDATE SET source = 'user'`, nodeID, tag)
	return err
}

// RemoveTag deletes a tag (auto or user) from one of the owner's files.
func (s *Store) RemoveTag(ctx context.Context, ownerID, nodeID uuid.UUID, raw string) error {
	if err := s.assertOwnedLive(ctx, ownerID, nodeID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM node_tags WHERE node_id = $1 AND tag = $2`, nodeID, normalizeTag(raw))
	return err
}

// TagsForNode returns a node's tags, verifying it belongs to the owner.
func (s *Store) TagsForNode(ctx context.Context, ownerID, nodeID uuid.UUID) ([]Tag, error) {
	if err := s.assertOwnedLive(ctx, ownerID, nodeID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tag, source FROM node_tags WHERE node_id = $1 ORDER BY tag`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.Name, &t.Source); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTags returns the owner's tags with how many live files carry each, most
// common first — the raw material for a tag cloud or filter list.
func (s *Store) ListTags(ctx context.Context, ownerID uuid.UUID) ([]TagCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT nt.tag, count(*)
		FROM node_tags nt
		JOIN nodes n ON n.id = nt.node_id
		WHERE n.owner_id = $1 AND n.trashed_at IS NULL
		GROUP BY nt.tag
		ORDER BY count(*) DESC, nt.tag`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// NodesByTag lists the owner's live files carrying a tag, newest first.
func (s *Store) NodesByTag(ctx context.Context, ownerID uuid.UUID, raw string, limit, offset int) ([]*Node, error) {
	limit = ClampSearchLimit(limit)
	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeCols+`
		`+nodeFrom+`
		JOIN node_tags nt ON nt.node_id = n.id
		WHERE n.owner_id = $1 AND nt.tag = $2 AND n.trashed_at IS NULL
		ORDER BY n.updated_at DESC
		LIMIT $3 OFFSET $4`,
		ownerID, normalizeTag(raw), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// assertOwnedLive confirms a node exists, belongs to the owner, and is not
// trashed — the ownership gate for every user-facing tag mutation.
func (s *Store) assertOwnedLive(ctx context.Context, ownerID, nodeID uuid.UUID) error {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM nodes WHERE id = $1 AND owner_id = $2 AND trashed_at IS NULL
		)`, nodeID, ownerID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}
