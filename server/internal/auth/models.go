// Package auth owns identity: users, passkeys, recovery codes and sessions.
//
// Design notes that shaped this package:
//
//   - Passkeys are the primary credential. There is no password.
//   - Because there is no password, LOCKOUT is the real risk: lose the
//     authenticator and you lose your own file server. Two independent escapes
//     exist — recovery codes, and the `cloudctl` CLI on the server itself.
//   - Sessions are server-side rows, not JWTs, so revocation is immediate.
package auth

import (
	"errors"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

var (
	ErrUserNotFound     = errors.New("user not found")
	ErrUserDisabled     = errors.New("user is disabled")
	ErrUserExists       = errors.New("username already taken")
	ErrNoCredentials    = errors.New("user has no registered passkeys")
	ErrSessionInvalid   = errors.New("session invalid or expired")
	ErrCeremonyNotFound = errors.New("ceremony not found or expired")
	ErrBadRecoveryCode  = errors.New("invalid recovery code")
	ErrRegistrationShut = errors.New("registration is closed")
)

type User struct {
	ID          uuid.UUID
	Username    string
	DisplayName string
	IsAdmin     bool
	QuotaBytes  *int64
	DisabledAt  *time.Time
	CreatedAt   time.Time
}

func (u *User) Disabled() bool { return u.DisabledAt != nil }

// --- webauthn.User implementation -------------------------------------------
//
// go-webauthn requires the user object to carry its credentials. webauthnUser
// wraps a User plus the credentials loaded for a particular ceremony, so the
// domain User type stays free of library concerns.

type webauthnUser struct {
	user  *User
	creds []webauthn.Credential
}

// WebAuthnID must be stable and must not be guessable-but-meaningful. The UUID
// bytes are used rather than the username, so renaming a user later does not
// invalidate their passkeys.
func (w webauthnUser) WebAuthnID() []byte { return w.user.ID[:] }

func (w webauthnUser) WebAuthnName() string { return w.user.Username }

func (w webauthnUser) WebAuthnDisplayName() string {
	if w.user.DisplayName != "" {
		return w.user.DisplayName
	}
	return w.user.Username
}

func (w webauthnUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

type Credential struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	CredentialID []byte
	Name         string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
}

type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Kind       string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

const (
	SessionKindWeb      = "web"
	SessionKindDevice   = "device"
	SessionKindRecovery = "recovery"
)
