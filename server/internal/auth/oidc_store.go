package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// userColsU is userCols qualified to the users table, for joins.
const userColsU = `u.id, u.username, u.display_name, u.is_admin, u.quota_bytes, u.disabled_at, u.created_at`

// FindUserByOIDC resolves an external identity to a local user, or ErrUserNotFound
// if this issuer+subject has never signed in. It records the login time as a side
// effect, best effort.
func (s *Store) FindUserByOIDC(ctx context.Context, issuer, subject string) (*User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT `+userColsU+`
		FROM oidc_identities oi JOIN users u ON u.id = oi.user_id
		WHERE oi.issuer = $1 AND oi.subject = $2`, issuer, subject))
	if err != nil {
		return nil, err
	}
	_, _ = s.pool.Exec(ctx,
		`UPDATE oidc_identities SET last_login_at = now() WHERE issuer = $1 AND subject = $2`, issuer, subject)
	return u, nil
}

// ProvisionOIDCUser creates a local user for a first-time SSO login and links the
// external identity to it. The user is non-admin — admin is bootstrapped by a
// passkey, never granted by an external provider. The username is derived from the
// email and de-duplicated, so two people named the same at different providers do
// not collide.
func (s *Store) ProvisionOIDCUser(ctx context.Context, issuer, subject, email, displayName string) (*User, error) {
	username, err := s.freeUsername(ctx, usernameFromEmail(email))
	if err != nil {
		return nil, err
	}
	if displayName == "" {
		displayName = username
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	id := uuid.New()
	u := &User{}
	err = tx.QueryRow(ctx, `
		INSERT INTO users (id, username, username_fold, display_name, is_admin)
		VALUES ($1, $2, lower($2), $3, false)
		RETURNING `+userCols, id, username, displayName).
		Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin, &u.QuotaBytes, &u.DisabledAt, &u.CreatedAt)
	if uniqueViolation(err) {
		return nil, ErrUserExists // lost a race; the caller retries the whole login
	}
	if err != nil {
		return nil, fmt.Errorf("create oidc user: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO oidc_identities (issuer, subject, user_id, email, last_login_at)
		VALUES ($1, $2, $3, $4, now())`, issuer, subject, u.ID, email); err != nil {
		if uniqueViolation(err) {
			return nil, ErrUserExists // this identity was provisioned concurrently
		}
		return nil, fmt.Errorf("link oidc identity: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return u, nil
}

// freeUsername returns base, or base with a numeric suffix, that is not yet taken.
func (s *Store) freeUsername(ctx context.Context, base string) (string, error) {
	for i := 0; i < 1000; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s%d", base, i+1)
		}
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM users WHERE username_fold = lower($1))`, candidate).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find a free username for %q", base)
}

// usernameFromEmail derives a username from an email's local part, keeping only
// characters safe in a username and falling back to "user" for a degenerate one.
func usernameFromEmail(email string) string {
	local, _, _ := strings.Cut(email, "@")
	local = strings.ToLower(strings.TrimSpace(local))

	var b strings.Builder
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return "user"
	}
	return out
}
