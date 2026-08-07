package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Username selection runs INSIDE the transaction, and the insert is retried
	// on a username collision rather than being reported as one.
	//
	// Picking the name outside meant two first-time logins from different
	// subjects whose emails share a local part (guru@a.com, guru@b.com) both
	// resolved to the same free candidate. The loser's insert hit the
	// username_fold unique constraint, which was translated to ErrUserExists —
	// and LoginOIDC reads that as "the other login provisioned MY identity", so
	// it re-resolves by (issuer, subject), finds nothing, and fails the login
	// with an opaque error that succeeds on retry.
	//
	// Two different constraints were being collapsed into one signal. They are
	// now handled where they differ: a username clash is ours to resolve by
	// picking another, and only a duplicate IDENTITY is a genuine race.
	base := usernameFromEmail(email)
	u := &User{}
	var username string
	for attempt := 0; ; attempt++ {
		username, err = freeUsername(ctx, tx, base, attempt)
		if err != nil {
			return nil, err
		}
		name := displayName
		if name == "" {
			name = username
		}

		err = tx.QueryRow(ctx, `
			INSERT INTO users (id, username, username_fold, display_name, is_admin)
			VALUES ($1, $2, lower($2), $3, false)
			RETURNING `+userCols, uuid.New(), username, name).
			Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin, &u.QuotaBytes, &u.DisabledAt, &u.CreatedAt)
		if err == nil {
			break
		}
		if !uniqueViolation(err) {
			return nil, fmt.Errorf("create oidc user: %w", err)
		}
		// Someone took this name between the check and the insert. A unique
		// violation aborts the transaction, so restart it and try the next
		// candidate rather than reporting a name clash as an identity race.
		if attempt >= maxUsernameAttempts {
			return nil, fmt.Errorf("could not find a free username for %q", base)
		}
		if err := tx.Rollback(ctx); err != nil {
			return nil, err
		}
		if tx, err = s.pool.Begin(ctx); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO oidc_identities (issuer, subject, user_id, email, last_login_at)
		VALUES ($1, $2, $3, $4, now())`, issuer, subject, u.ID, email); err != nil {
		if uniqueViolation(err) {
			// THIS is the real race: the same (issuer, subject) was provisioned
			// concurrently, so the caller re-resolving by identity will find it.
			return nil, ErrUserExists
		}
		return nil, fmt.Errorf("link oidc identity: %w", err)
	}

	return u, tx.Commit(ctx)
}

// maxUsernameAttempts bounds the search for a free name, so a pathological
// collection of near-identical addresses cannot spin here forever.
const maxUsernameAttempts = 1000

// freeUsername returns the nth candidate name derived from base that is not
// currently taken. It reads through the caller's transaction so the check and the
// insert that follows see the same snapshot.
func freeUsername(ctx context.Context, tx pgx.Tx, base string, from int) (string, error) {
	for i := from; i < maxUsernameAttempts; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s%d", base, i+1)
		}
		var exists bool
		if err := tx.QueryRow(ctx,
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
