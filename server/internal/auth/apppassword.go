package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrAppPasswordInvalid = errors.New("invalid app password")
	ErrAppPasswordExists  = errors.New("an app password with that name already exists")
)

// AppPassword is a credential for clients that cannot run a WebAuthn ceremony.
type AppPassword struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
}

// App password format: pcap_<lookup>_<secret>
//
//   - The "pcap_" prefix makes a leaked credential greppable. Secret scanners
//     and a human reading a config file can both recognise it for what it is,
//     which is the difference between a leak found in minutes and one found
//     never.
//   - The lookup half is stored in clear and indexed, so verifying a password
//     hashes ONE candidate instead of every password the user owns. A WebDAV
//     client issues a great many requests, and argon2id at 64 MiB per request
//     per candidate is not something to do in a loop.
//   - The secret half is 128 bits, hashed with argon2id and never recoverable.
const (
	appPasswordPrefix   = "pcap_"
	appPasswordLookupSz = 8  // bytes -> 16 hex chars
	appPasswordSecretSz = 16 // bytes -> 32 hex chars, 128 bits
)

// GenerateAppPassword returns the plaintext credential and its two parts.
//
// Both halves are hex, not base64url. base64url's alphabet contains the
// underscore that separates the fields, so a lookup half could contain the
// delimiter and be split in the wrong place — producing a credential that
// verifies inconsistently depending on the bytes it happened to be given. Hex
// costs a few characters of length and removes the whole class of problem.
func GenerateAppPassword() (plaintext, lookup string, err error) {
	lookupRaw := make([]byte, appPasswordLookupSz)
	secretRaw := make([]byte, appPasswordSecretSz)
	if _, err := rand.Read(lookupRaw); err != nil {
		return "", "", fmt.Errorf("generate app password: %w", err)
	}
	if _, err := rand.Read(secretRaw); err != nil {
		return "", "", fmt.Errorf("generate app password: %w", err)
	}

	lookup = hex.EncodeToString(lookupRaw)
	secret := hex.EncodeToString(secretRaw)
	return appPasswordPrefix + lookup + "_" + secret, lookup, nil
}

// splitAppPassword pulls the lookup and secret halves out of a plaintext value.
func splitAppPassword(plaintext string) (lookup, secret string, ok bool) {
	rest, found := strings.CutPrefix(plaintext, appPasswordPrefix)
	if !found {
		return "", "", false
	}
	lookup, secret, found = strings.Cut(rest, "_")
	// Both halves are hex of a known length. Checking that here means a
	// malformed value is rejected before it reaches the database, rather than
	// becoming a lookup miss that costs a query.
	if !found || len(lookup) != appPasswordLookupSz*2 || len(secret) != appPasswordSecretSz*2 {
		return "", "", false
	}
	return lookup, secret, true
}

// --- store ------------------------------------------------------------------

// CreateAppPassword issues a credential and returns the plaintext ONCE.
func (s *Store) CreateAppPassword(ctx context.Context, userID uuid.UUID, name string, ttl time.Duration) (*AppPassword, string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, "", errors.New("app password name is required")
	}

	plaintext, lookup, err := GenerateAppPassword()
	if err != nil {
		return nil, "", err
	}
	_, secret, _ := splitAppPassword(plaintext)

	// HashSecret, not HashRecoveryCode: the latter normalises case and strips
	// characters so a human can retype a printed code, which for a
	// machine-generated secret would throw away most of its entropy.
	hash, err := HashSecret(secret)
	if err != nil {
		return nil, "", err
	}

	var expires any
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}

	ap := &AppPassword{UserID: userID, Name: name}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO app_passwords (user_id, name, secret_hash, lookup_id, expires_at)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, created_at, expires_at`,
		userID, name, hash, lookup, expires,
	).Scan(&ap.ID, &ap.CreatedAt, &ap.ExpiresAt)
	if err != nil {
		return nil, "", fmt.Errorf("create app password: %w", err)
	}
	return ap, plaintext, nil
}

func (s *Store) ListAppPasswords(ctx context.Context, userID uuid.UUID) ([]*AppPassword, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, name, created_at, last_used_at, expires_at
		FROM app_passwords
		WHERE user_id = $1 AND revoked_at IS NULL
		ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*AppPassword
	for rows.Next() {
		ap := &AppPassword{}
		if err := rows.Scan(&ap.ID, &ap.UserID, &ap.Name, &ap.CreatedAt, &ap.LastUsedAt, &ap.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

func (s *Store) RevokeAppPassword(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE app_passwords SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("app password not found")
	}
	return nil
}

// RevokeAllAppPasswords is part of `cloudctl user reset-auth`: an account being
// recovered must not leave a working credential behind for whoever caused the
// lockout.
func (s *Store) RevokeAllAppPasswords(ctx context.Context, userID uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE app_passwords SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// VerifyAppPassword resolves a plaintext credential to its user.
//
// The lookup half selects one row; argon2id on the secret half is what actually
// authenticates. Both a malformed value and a wrong secret return the same
// error, so the response cannot be used to probe which app passwords exist.
func (s *Store) VerifyAppPassword(ctx context.Context, plaintext string) (*User, error) {
	lookup, secret, ok := splitAppPassword(plaintext)
	if !ok {
		return nil, ErrAppPasswordInvalid
	}

	var (
		id   uuid.UUID
		hash string
		u    = &User{}
	)
	err := s.pool.QueryRow(ctx, `
		SELECT a.id, a.secret_hash,
		       u.id, u.username, u.display_name, u.is_admin, u.quota_bytes, u.disabled_at, u.created_at
		FROM app_passwords a JOIN users u ON u.id = a.user_id
		WHERE a.lookup_id = $1
		  AND a.revoked_at IS NULL
		  AND (a.expires_at IS NULL OR a.expires_at > now())`,
		lookup,
	).Scan(&id, &hash,
		&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin, &u.QuotaBytes, &u.DisabledAt, &u.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppPasswordInvalid
	}
	if err != nil {
		return nil, err
	}
	if !VerifySecret(secret, hash) {
		return nil, ErrAppPasswordInvalid
	}
	if u.Disabled() {
		return nil, ErrUserDisabled
	}

	// Best effort: a failed timestamp update must not fail the request. The
	// column exists so a user can spot a credential that is still in use
	// somewhere they forgot about.
	if _, err := s.pool.Exec(ctx, `UPDATE app_passwords SET last_used_at = now() WHERE id = $1`, id); err != nil {
		return u, nil
	}
	return u, nil
}
