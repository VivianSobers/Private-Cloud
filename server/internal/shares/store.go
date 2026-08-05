// Package shares implements public, read-only share links — the first surface
// in the system reachable without an account.
//
// Everything here is written defensively on purpose: a bug in the authenticated
// API leaks one user's data to that user, but a bug here leaks it to the open
// internet. So a share is a database row (revocable instantly, not a signed
// token that outlives its revocation), the token is stored hashed, and a served
// response carries the file's own bytes and nothing about the owner, the path,
// or the rest of their storage.
package shares

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no share matches — deliberately the same answer
// for "never existed" and "you may not see it", so the public surface never
// distinguishes the two.
var ErrNotFound = errors.New("share not found")

// Store persists shares.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const shareCols = `id, node_id, owner_id, token_hash, unlock_key, password_hash,
	expires_at, max_downloads, download_count, created_at, revoked_at`

func scanShare(row pgx.Row) (*Share, error) {
	var (
		s  Share
		pw *string
	)
	err := row.Scan(&s.ID, &s.NodeID, &s.OwnerID, &s.TokenHash, &s.UnlockKey, &pw,
		&s.ExpiresAt, &s.MaxDownloads, &s.DownloadCount, &s.CreatedAt, &s.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if pw != nil {
		s.PasswordHash = *pw
	}
	return &s, nil
}

// CreateInput carries a new share's fields. The token has already been hashed
// and the password already argon2-encoded by the service — the store never sees
// a plaintext secret.
type CreateInput struct {
	NodeID       uuid.UUID
	OwnerID      uuid.UUID
	TokenHash    []byte
	UnlockKey    []byte
	PasswordHash string
	ExpiresAt    *time.Time
	MaxDownloads *int64
}

// Create records a new share.
func (st *Store) Create(ctx context.Context, in CreateInput) (*Share, error) {
	var pw *string
	if in.PasswordHash != "" {
		pw = &in.PasswordHash
	}
	row := st.pool.QueryRow(ctx, `
		INSERT INTO shares (node_id, owner_id, token_hash, unlock_key, password_hash, expires_at, max_downloads)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+shareCols,
		in.NodeID, in.OwnerID, in.TokenHash, in.UnlockKey, pw, in.ExpiresAt, in.MaxDownloads)
	sh, err := scanShare(row)
	if err != nil {
		return nil, fmt.Errorf("create share: %w", err)
	}
	return sh, nil
}

// FindByTokenHash resolves the hash of a presented token to its share.
//
// This is the only lookup the public plane makes, and it deliberately does no
// validity filtering: whether a share is revoked, expired or spent is decided by
// the service against a single clock reading, so the reasons can be reported
// precisely rather than collapsed into one "not found" that also hides typos.
func (st *Store) FindByTokenHash(ctx context.Context, tokenHash []byte) (*Share, error) {
	return scanShare(st.pool.QueryRow(ctx,
		`SELECT `+shareCols+` FROM shares WHERE token_hash = $1`, tokenHash))
}

// TryIncrementDownload counts one download, but ONLY if the share is still
// serveable at this instant. The whole validity test lives inside the UPDATE's
// WHERE clause so the cap is enforced atomically: two downloads racing the last
// remaining count cannot both succeed, because the row is checked and bumped in
// one statement. Returns false when the share has just been revoked, expired, or
// spent — in which case the caller must not serve the bytes.
func (st *Store) TryIncrementDownload(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := st.pool.Exec(ctx, `
		UPDATE shares SET download_count = download_count + 1
		WHERE id = $1
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
		  AND (max_downloads IS NULL OR download_count < max_downloads)`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Revoke kills a link immediately, scoped to its owner. Idempotent: revoking an
// already-revoked share reports false (nothing changed) rather than erroring, so
// a double click is harmless.
func (st *Store) Revoke(ctx context.Context, ownerID, id uuid.UUID) (bool, error) {
	tag, err := st.pool.Exec(ctx, `
		UPDATE shares SET revoked_at = now()
		WHERE id = $1 AND owner_id = $2 AND revoked_at IS NULL`, id, ownerID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// OwnerShare is a share as its creator sees it: the row plus the file it points
// at. The node name and path are safe to show HERE because the viewer owns them
// — this is the management listing, not the public surface.
type OwnerShare struct {
	Share
	NodeName    string
	NodePath    string
	NodeTrashed bool
}

// ListForOwner returns every share a user has created, newest first.
func (st *Store) ListForOwner(ctx context.Context, ownerID uuid.UUID) ([]OwnerShare, error) {
	rows, err := st.pool.Query(ctx, `
		SELECT s.id, s.node_id, s.owner_id, s.token_hash, s.unlock_key, s.password_hash,
		       s.expires_at, s.max_downloads, s.download_count, s.created_at, s.revoked_at,
		       n.name, n.path, (n.trashed_at IS NOT NULL)
		FROM shares s
		JOIN nodes n ON n.id = s.node_id
		WHERE s.owner_id = $1
		ORDER BY s.created_at DESC`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OwnerShare
	for rows.Next() {
		var (
			os OwnerShare
			pw *string
		)
		if err := rows.Scan(&os.ID, &os.NodeID, &os.OwnerID, &os.TokenHash, &os.UnlockKey, &pw,
			&os.ExpiresAt, &os.MaxDownloads, &os.DownloadCount, &os.CreatedAt, &os.RevokedAt,
			&os.NodeName, &os.NodePath, &os.NodeTrashed); err != nil {
			return nil, err
		}
		if pw != nil {
			os.PasswordHash = *pw
		}
		out = append(out, os)
	}
	return out, rows.Err()
}

// DeleteStale hard-removes shares that have been revoked or expired for longer
// than grace, bounded per pass. The grace keeps a just-revoked link visible in
// the owner's list for a while — "revoked 2 minutes ago" is more reassuring than
// a link that silently vanishes — before the row is finally reclaimed.
func (st *Store) DeleteStale(ctx context.Context, grace time.Duration, limit int) (int64, error) {
	tag, err := st.pool.Exec(ctx, `
		DELETE FROM shares
		WHERE id IN (
			SELECT id FROM shares
			WHERE (revoked_at IS NOT NULL AND revoked_at < now() - $1::interval)
			   OR (expires_at IS NOT NULL AND expires_at < now() - $1::interval)
			ORDER BY created_at
			LIMIT $2
		)`, grace.String(), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Share is one public link to one file.
type Share struct {
	ID      uuid.UUID
	NodeID  uuid.UUID
	OwnerID uuid.UUID

	// TokenHash is SHA-256 of the URL token; the token itself is never stored.
	TokenHash []byte
	// UnlockKey is a per-share secret, never sent to a client, used to prove a
	// password was verified without keeping per-visitor session state.
	UnlockKey []byte

	// PasswordHash is an argon2id PHC string, or empty for no password.
	PasswordHash string

	ExpiresAt     *time.Time
	MaxDownloads  *int64
	DownloadCount int64

	CreatedAt time.Time
	RevokedAt *time.Time
}

// HasPassword reports whether a password gates this share.
func (s *Share) HasPassword() bool { return s.PasswordHash != "" }

// Revoked reports whether the owner has killed this link.
func (s *Share) Revoked() bool { return s.RevokedAt != nil }

// Expired reports whether the link's time has passed.
func (s *Share) Expired(now time.Time) bool {
	return s.ExpiresAt != nil && !now.Before(*s.ExpiresAt)
}

// CapReached reports whether the download cap is spent. The check is advisory
// for display; the atomic increment in the store is what actually enforces it
// against concurrent downloads.
func (s *Share) CapReached() bool {
	return s.MaxDownloads != nil && s.DownloadCount >= *s.MaxDownloads
}

// Active reports whether a link may still be served: not revoked, not expired,
// not over its cap.
func (s *Share) Active(now time.Time) bool {
	return !s.Revoked() && !s.Expired(now) && !s.CapReached()
}
