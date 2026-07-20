package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

// ceremonyTTL bounds how long a started registration/login may take to finish.
// Long enough for a user to find their phone; short enough that abandoned
// challenges do not accumulate.
const ceremonyTTL = 5 * time.Minute

// recoverySessionTTL is deliberately short. A recovery session exists only to
// let you enrol a new passkey — it is not a way to work around not having one.
const recoverySessionTTL = 15 * time.Minute

type Config struct {
	RPDisplayName string
	RPID          string
	RPOrigins     []string
	SessionTTL    time.Duration
}

type Service struct {
	store *Store
	wa    *webauthn.WebAuthn
	cfg   Config
	log   *slog.Logger
}

func NewService(store *Store, cfg Config, log *slog.Logger) (*Service, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPDisplayName: cfg.RPDisplayName,
		RPID:          cfg.RPID,
		RPOrigins:     cfg.RPOrigins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			// Prefer a resident/discoverable credential so the browser can
			// later offer usernameless login, but don't require it — some
			// hardware keys have very limited resident-credential slots.
			ResidentKey: protocol.ResidentKeyRequirementPreferred,
			// Require user verification (PIN/biometric), which is what makes a
			// passkey genuinely multi-factor rather than just possession.
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure webauthn: %w", err)
	}
	return &Service{store: store, wa: wa, cfg: cfg, log: log}, nil
}

// --- registration -----------------------------------------------------------

// BeginRegistration starts enrolling a passkey.
//
// Two legitimate paths, and nothing else:
//
//  1. Bootstrap — the users table is empty, so the first passkey to arrive
//     creates the admin. This is the only unauthenticated way to create a user.
//  2. Adding a passkey to your own account, which requires being signed in.
//
// Creating additional users is done from the server with `cloudctl user
// create`, which issues recovery codes. That keeps account creation off the
// public surface entirely instead of relying on an invite-token flow.
func (s *Service) BeginRegistration(ctx context.Context, username string, current *User) (*protocol.CredentialCreation, uuid.UUID, error) {
	var (
		target   *User
		pending  string
		targetID *uuid.UUID
	)

	switch {
	case current != nil:
		target = current
		targetID = &current.ID

	default:
		n, err := s.store.CountUsers(ctx)
		if err != nil {
			return nil, uuid.Nil, err
		}
		if n > 0 {
			return nil, uuid.Nil, ErrRegistrationShut
		}
		if username == "" {
			return nil, uuid.Nil, fmt.Errorf("username is required")
		}
		// The user row is NOT created yet. Creating it here would leave an
		// orphaned, credential-less account behind every abandoned ceremony —
		// and that account would occupy the username. The row is created on
		// successful finish instead; the ceremony carries the username.
		pending = username
		target = &User{ID: uuid.New(), Username: username, DisplayName: username}
	}

	creds, err := s.credentialsFor(ctx, targetID)
	if err != nil {
		return nil, uuid.Nil, err
	}

	options, sessionData, err := s.wa.BeginRegistration(
		webauthnUser{user: target, creds: creds},
		// Exclude already-registered credentials so an authenticator cannot be
		// enrolled twice on one account.
		webauthn.WithExclusions(credentialDescriptors(creds)),
	)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("begin registration: %w", err)
	}

	id, err := s.store.CreateCeremony(ctx, targetID, "registration", pending, sessionData, ceremonyTTL)
	if err != nil {
		return nil, uuid.Nil, err
	}
	return options, id, nil
}

// FinishRegistration verifies the attestation and persists the credential.
// On the bootstrap path it also creates the user and their recovery codes,
// which are returned in plaintext — the only time they are ever available.
func (s *Service) FinishRegistration(ctx context.Context, ceremonyID uuid.UUID, credName string, r *http.Request) (*User, []string, error) {
	userID, pendingUsername, sessionData, err := s.store.TakeCeremony(ctx, ceremonyID, "registration")
	if err != nil {
		return nil, nil, err
	}

	var (
		user      *User
		bootstrap bool
		handleID  uuid.UUID
	)
	if userID != nil {
		user, err = s.store.GetUserByID(ctx, *userID)
		if err != nil {
			return nil, nil, err
		}
	} else {
		bootstrap = true
		// Reconstruct the exact identity the challenge was issued against.
		// sessionData.UserID is the user handle the authenticator has now
		// baked into the credential, so the users row MUST be created with
		// this same UUID — see Store.CreateUser.
		if len(sessionData.UserID) != len(handleID) {
			return nil, nil, fmt.Errorf("ceremony carries a malformed user handle")
		}
		copy(handleID[:], sessionData.UserID)
		user = &User{ID: handleID, Username: pendingUsername, DisplayName: pendingUsername}
	}

	creds, err := s.credentialsFor(ctx, userID)
	if err != nil {
		return nil, nil, err
	}

	credential, err := s.wa.FinishRegistration(webauthnUser{user: user, creds: creds}, *sessionData, r)
	if err != nil {
		return nil, nil, fmt.Errorf("verify registration: %w", err)
	}

	var recoveryCodes []string
	if bootstrap {
		// Attestation verified before any row is written, so a failed ceremony
		// leaves no trace.
		//
		// handleID — not a fresh UUID — because the authenticator has already
		// baked it into the credential as the user handle, and FinishLogin
		// validates that handle against the stored user. Getting this wrong
		// produces an admin who can register but can never sign in again.
		created, err := s.store.CreateUser(ctx, handleID, pendingUsername, pendingUsername, true)
		if err != nil {
			return nil, nil, err
		}
		user = created

		recoveryCodes, err = s.RegenerateRecoveryCodes(ctx, user.ID)
		if err != nil {
			return nil, nil, err
		}
		s.log.Info("bootstrap admin created", "user_id", user.ID, "username", user.Username)
	}

	if err := s.store.AddCredential(ctx, user.ID, credName, credential); err != nil {
		return nil, nil, err
	}
	return user, recoveryCodes, nil
}

// --- login ------------------------------------------------------------------

func (s *Service) BeginLogin(ctx context.Context, username string) (*protocol.CredentialAssertion, uuid.UUID, error) {
	user, err := s.store.GetUserByUsername(ctx, username)
	if err != nil {
		return nil, uuid.Nil, err
	}
	if user.Disabled() {
		return nil, uuid.Nil, ErrUserDisabled
	}

	creds, err := s.store.CredentialsForUser(ctx, user.ID)
	if err != nil {
		return nil, uuid.Nil, err
	}
	if len(creds) == 0 {
		return nil, uuid.Nil, ErrNoCredentials
	}

	options, sessionData, err := s.wa.BeginLogin(webauthnUser{user: user, creds: creds})
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("begin login: %w", err)
	}

	id, err := s.store.CreateCeremony(ctx, &user.ID, "login", "", sessionData, ceremonyTTL)
	if err != nil {
		return nil, uuid.Nil, err
	}
	return options, id, nil
}

func (s *Service) FinishLogin(ctx context.Context, ceremonyID uuid.UUID, r *http.Request) (*User, error) {
	userID, _, sessionData, err := s.store.TakeCeremony(ctx, ceremonyID, "login")
	if err != nil {
		return nil, err
	}
	if userID == nil {
		return nil, ErrCeremonyNotFound
	}

	user, err := s.store.GetUserByID(ctx, *userID)
	if err != nil {
		return nil, err
	}
	// Re-checked here, not just at BeginLogin: an account disabled mid-ceremony
	// must not complete a login.
	if user.Disabled() {
		return nil, ErrUserDisabled
	}

	creds, err := s.store.CredentialsForUser(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	credential, err := s.wa.FinishLogin(webauthnUser{user: user, creds: creds}, *sessionData, r)
	if err != nil {
		return nil, fmt.Errorf("verify login: %w", err)
	}

	if credential.Authenticator.CloneWarning {
		// Not fatal — sign counters are unreliable across some platform
		// authenticators — but it is exactly the kind of thing you want a
		// record of if an account is later found compromised.
		s.log.Warn("authenticator clone warning",
			"user_id", user.ID, "username", user.Username)
	}

	if err := s.store.UpdateCredentialUsage(ctx, credential.ID,
		credential.Authenticator.SignCount, credential.Authenticator.CloneWarning); err != nil {
		s.log.Warn("could not persist credential usage", "error", err, "user_id", user.ID)
	}
	return user, nil
}

// --- recovery ---------------------------------------------------------------

// RedeemRecoveryCode trades a printed code for a short-lived recovery session.
func (s *Service) RedeemRecoveryCode(ctx context.Context, username, code string) (*User, error) {
	user, err := s.store.GetUserByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	if user.Disabled() {
		return nil, ErrUserDisabled
	}
	if err := s.store.ConsumeRecoveryCode(ctx, user.ID, code); err != nil {
		return nil, err
	}
	s.log.Warn("recovery code redeemed", "user_id", user.ID, "username", user.Username)
	return user, nil
}

// RegenerateRecoveryCodes issues a fresh set, invalidating all previous codes.
// Returns plaintext for one-time display; only hashes are stored.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID uuid.UUID) ([]string, error) {
	codes, err := GenerateRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, err
	}

	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		h, err := HashRecoveryCode(c)
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}

	if err := s.store.ReplaceRecoveryCodes(ctx, userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// --- sessions ---------------------------------------------------------------

// NewSessionToken mints a session and returns the plaintext token, which is
// stored only in the client's cookie — the database keeps its SHA-256.
//
// SHA-256 rather than argon2 is correct here: the token is 256 bits of CSPRNG
// output, so there is no low-entropy secret to slow an attacker down over, and
// this runs on every authenticated request.
func (s *Service) NewSessionToken(ctx context.Context, user *User, kind, userAgent string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	ttl := s.cfg.SessionTTL
	if kind == SessionKindRecovery {
		ttl = recoverySessionTTL
	}
	expires := time.Now().Add(ttl)

	if _, err := s.store.CreateSession(ctx, user.ID, hashToken(token), kind, userAgent, expires); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// Authenticate resolves a session token. Expiry and revocation are enforced in
// SQL, so there is no path where a caller forgets to check them.
func (s *Service) Authenticate(ctx context.Context, token string) (*Session, *User, error) {
	if token == "" {
		return nil, nil, ErrSessionInvalid
	}
	sess, user, err := s.store.SessionUser(ctx, hashToken(token))
	if err != nil {
		return nil, nil, err
	}
	if user.Disabled() {
		return nil, nil, ErrUserDisabled
	}
	return sess, user, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.store.RevokeSessionByTokenHash(ctx, hashToken(token))
}

func (s *Service) Store() *Store { return s.store }

// --- helpers ----------------------------------------------------------------

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func (s *Service) credentialsFor(ctx context.Context, userID *uuid.UUID) ([]webauthn.Credential, error) {
	if userID == nil {
		return nil, nil
	}
	return s.store.CredentialsForUser(ctx, *userID)
}

func credentialDescriptors(creds []webauthn.Credential) []protocol.CredentialDescriptor {
	out := make([]protocol.CredentialDescriptor, 0, len(creds))
	for _, c := range creds {
		out = append(out, c.Descriptor())
	}
	return out
}
