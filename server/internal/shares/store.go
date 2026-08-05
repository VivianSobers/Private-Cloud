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
	"time"

	"github.com/google/uuid"
)

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
