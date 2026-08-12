package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Admin user management (Phase 7).
//
// cloudctl already does most of this on the server itself, and that stays the
// break-glass path — it needs shell access, which already implies database and
// file access, so it weakens nothing. These endpoints exist so the same work can
// be done from the admin console without an SSH session.

// ErrLastAdmin refuses to remove the last administrator. (ErrUserNotFound is
// declared in models.go alongside the other domain errors.)
var ErrLastAdmin = errors.New("cannot remove the last admin")

// UserPatch is a partial change to an account. Nil fields are left alone; the
// difference between "do not touch the quota" and "clear the quota" cannot be
// expressed by a plain int64.
type UserPatch struct {
	DisplayName *string
	IsAdmin     *bool
	Disabled    *bool
	// QuotaBytes set to a pointer-to-nil clears the quota (unlimited); set to a
	// value sets it.
	QuotaBytes **int64
}

// UpdateUser applies a partial change to an account.
//
// Demoting or disabling the last admin is refused. Locking every administrator
// out of their own server is not a state an API should be able to reach in one
// request — the recovery from it is a shell and a SQL prompt.
func (s *Store) UpdateUser(ctx context.Context, id uuid.UUID, patch UserPatch) (*User, error) {
	if (patch.IsAdmin != nil && !*patch.IsAdmin) || (patch.Disabled != nil && *patch.Disabled) {
		last, err := s.isLastActiveAdmin(ctx, id)
		if err != nil {
			return nil, err
		}
		if last {
			return nil, ErrLastAdmin
		}
	}

	var (
		displayName any
		isAdmin     any
		setQuota    = patch.QuotaBytes != nil
		quota       *int64
		disabled    any
	)
	if patch.DisplayName != nil {
		displayName = strings.TrimSpace(*patch.DisplayName)
	}
	if patch.IsAdmin != nil {
		isAdmin = *patch.IsAdmin
	}
	if patch.Disabled != nil {
		disabled = *patch.Disabled
	}
	if setQuota {
		quota = *patch.QuotaBytes
	}

	u := &User{}
	err := s.pool.QueryRow(ctx, `
		UPDATE users SET
			display_name = coalesce($2, display_name),
			is_admin     = coalesce($3, is_admin),
			quota_bytes  = CASE WHEN $4 THEN $5 ELSE quota_bytes END,
			disabled_at  = CASE
				WHEN $6::boolean IS NULL THEN disabled_at
				WHEN $6 THEN coalesce(disabled_at, now())
				ELSE NULL
			END,
			updated_at = now()
		WHERE id = $1
		RETURNING id, username, display_name, is_admin, quota_bytes, disabled_at, created_at`,
		id, displayName, isAdmin, setQuota, quota, disabled,
	).Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin, &u.QuotaBytes, &u.DisabledAt, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}

	// Disabling has to take effect NOW, not when the session happens to expire.
	// A disabled account whose browser tab keeps working is not disabled.
	if patch.Disabled != nil && *patch.Disabled {
		if _, err := s.RevokeAllSessions(ctx, id); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// DisableUser is what DELETE /admin/users/{id} does.
//
// It disables and revokes; it does NOT delete. Deleting a user cascades their
// files away, and "remove this person's access" almost never means "destroy
// everything they ever uploaded". Making that irreversible step reachable from a
// console button is how it happens by accident.
func (s *Store) DisableUser(ctx context.Context, id uuid.UUID) error {
	disabled := true
	_, err := s.UpdateUser(ctx, id, UserPatch{Disabled: &disabled})
	return err
}

// isLastActiveAdmin reports whether this user is the only enabled admin left.
func (s *Store) isLastActiveAdmin(ctx context.Context, id uuid.UUID) (bool, error) {
	var isAdmin bool
	err := s.pool.QueryRow(ctx,
		`SELECT is_admin FROM users WHERE id = $1 AND disabled_at IS NULL`, id).Scan(&isAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already gone or already disabled — not the last admin by definition.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !isAdmin {
		return false, nil
	}

	var others int
	err = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE is_admin AND disabled_at IS NULL AND id <> $1`,
		id).Scan(&others)
	if err != nil {
		return false, err
	}
	return others == 0, nil
}
