package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
)

const (
	userCtxKey    ctxKey = 100
	sessionCtxKey ctxKey = 101

	// ceremonyCookie carries the in-flight WebAuthn ceremony ID between the
	// /begin and /finish calls. It lives in a cookie because go-webauthn reads
	// the credential from the request body, leaving nowhere in the body for it.
	ceremonyCookie = "pc_ceremony"
)

// CurrentUser returns the authenticated user, or nil.
func CurrentUser(ctx context.Context) *auth.User {
	u, _ := ctx.Value(userCtxKey).(*auth.User)
	return u
}

// withUser attaches an authenticated user to a context. Used by the WebDAV
// endpoint, which authenticates with Basic auth rather than a session and so
// has no auth.Session to carry alongside.
func withUser(ctx context.Context, u *auth.User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

func currentSession(ctx context.Context) *auth.Session {
	s, _ := ctx.Value(sessionCtxKey).(*auth.Session)
	return s
}

// --- middleware -------------------------------------------------------------

// requireAuth rejects unauthenticated requests.
//
// Wraps individual handlers rather than the whole mux: auth endpoints and
// health probes must stay reachable, and an allowlist that has to be kept in
// sync with the routing table is a bug waiting to happen. Opt-in per route
// means a new protected route cannot accidentally inherit "public".
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := sessionTokenFrom(r, s.cookieName)

		sess, user, err := s.auth.Authenticate(r.Context(), token)
		if err != nil {
			// Deliberately uniform: never reveal whether the session was
			// unknown, expired, revoked, or belonged to a disabled account.
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}

		// A recovery session may only be used to enrol a new passkey. Letting
		// it reach the rest of the API would make a printed code equivalent to
		// a passkey, defeating the point of having passkeys.
		if sess.Kind == auth.SessionKindRecovery && !isRecoveryAllowedPath(r.URL.Path) {
			writeError(w, r, http.StatusForbidden, "recovery_session_limited",
				"this recovery session may only be used to register a new passkey")
			return
		}

		// Cheap freshness for the "last seen" column; not worth a write on
		// every single request.
		if time.Since(sess.LastSeenAt) > 5*time.Minute {
			if err := s.auth.Store().TouchSession(r.Context(), sess.ID); err != nil {
				s.log.Warn("touch session", "error", err)
			}
		}

		ctx := context.WithValue(r.Context(), userCtxKey, user)
		ctx = context.WithValue(ctx, sessionCtxKey, sess)
		next(w, r.WithContext(ctx))
	}
}

func isRecoveryAllowedPath(path string) bool {
	switch path {
	case "/api/v1/auth/register/begin",
		"/api/v1/auth/register/finish",
		"/api/v1/auth/me",
		"/api/v1/auth/logout",
		"/api/v1/auth/recovery/regenerate":
		return true
	}
	return false
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if u := CurrentUser(r.Context()); u == nil || !u.IsAdmin {
			writeError(w, r, http.StatusForbidden, "forbidden", "admin privileges required")
			return
		}
		next(w, r)
	})
}

// sessionTokenFrom prefers the cookie (browsers) and falls back to a bearer
// token (CLI and WebDAV clients in later slices).
func sessionTokenFrom(r *http.Request, cookieName string) string {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return c.Value
	}
	const prefix = "Bearer "
	if h := r.Header.Get("Authorization"); len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}

// --- cookies ----------------------------------------------------------------

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:  s.cookieName,
		Value: token,
		Path:  "/",
		// HttpOnly: JavaScript must never be able to read the session token,
		// so an XSS bug cannot become account takeover.
		HttpOnly: true,
		Secure:   s.cookieSecure,
		// Lax rather than Strict: Strict breaks following a link into the app
		// from elsewhere. Lax still blocks cross-site POSTs, which is the CSRF
		// vector that matters for a JSON API.
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/",
		HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteLaxMode,
		MaxAge: -1,
	})
}

func (s *Server) setCeremonyCookie(w http.ResponseWriter, id uuid.UUID) {
	http.SetCookie(w, &http.Cookie{
		Name: ceremonyCookie, Value: id.String(), Path: "/api/v1/auth",
		HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteLaxMode,
		MaxAge: 300, // matches the server-side ceremony TTL
	})
}

func ceremonyIDFrom(r *http.Request) (uuid.UUID, bool) {
	c, err := r.Cookie(ceremonyCookie)
	if err != nil {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(c.Value)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// --- handlers ---------------------------------------------------------------

// handleAuthStatus tells an unauthenticated client what to render: the
// bootstrap form, or the login form.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.auth.Store().CountUsers(r.Context())
	if err != nil {
		s.serverError(w, r, "count users", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"bootstrap_required": n == 0,
		"user_count":         n,
	})
}

type beginRegistrationRequest struct {
	Username string `json:"username"`
}

func (s *Server) handleRegisterBegin(w http.ResponseWriter, r *http.Request) {
	var req beginRegistrationRequest
	// A missing body is fine when adding a passkey to an existing account.
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Optional session: present means "add a passkey to my account", absent
	// means "bootstrap". The service decides which is permitted.
	var current *auth.User
	if _, u, err := s.auth.Authenticate(r.Context(), sessionTokenFrom(r, s.cookieName)); err == nil {
		current = u
	}

	options, ceremonyID, err := s.auth.BeginRegistration(r.Context(), req.Username, current)
	switch {
	case errors.Is(err, auth.ErrRegistrationShut):
		writeError(w, r, http.StatusForbidden, "registration_closed",
			"an account already exists; sign in first, or create users with `cloudctl user create`")
		return
	case errors.Is(err, auth.ErrUserExists):
		writeError(w, r, http.StatusConflict, "username_taken", "that username is taken")
		return
	case err != nil:
		s.serverError(w, r, "begin registration", err)
		return
	}

	s.setCeremonyCookie(w, ceremonyID)
	writeJSON(w, r, http.StatusOK, options)
}

func (s *Server) handleRegisterFinish(w http.ResponseWriter, r *http.Request) {
	ceremonyID, ok := ceremonyIDFrom(r)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "no_ceremony", "no registration in progress")
		return
	}
	s.clearCookie(w, ceremonyCookie)

	name := r.URL.Query().Get("name")
	if name == "" {
		name = "passkey"
	}

	user, recoveryCodes, err := s.auth.FinishRegistration(r.Context(), ceremonyID, name, r)
	switch {
	case errors.Is(err, auth.ErrCeremonyNotFound):
		writeError(w, r, http.StatusBadRequest, "ceremony_expired", "registration expired; start again")
		return
	case errors.Is(err, auth.ErrUserExists):
		writeError(w, r, http.StatusConflict, "username_taken", "that username is taken")
		return
	case err != nil:
		// Attestation failures are client errors, not server errors — a wrong
		// origin or a cancelled prompt lands here.
		s.log.Warn("registration failed", "error", err, "request_id", RequestID(r.Context()))
		writeError(w, r, http.StatusBadRequest, "registration_failed", "could not verify the passkey")
		return
	}

	resp := map[string]any{"user": userJSON(user)}

	// Recovery codes are returned EXACTLY ONCE, on bootstrap. They are not
	// retrievable afterwards — only regenerable, which invalidates the old set.
	if len(recoveryCodes) > 0 {
		resp["recovery_codes"] = recoveryCodes
		resp["recovery_codes_notice"] = "Store these now. They are shown once and cannot be recovered. Print them."

		// Sign the new admin in.
		token, expires, err := s.auth.NewSessionToken(r.Context(), user, auth.SessionKindWeb, r.UserAgent())
		if err != nil {
			s.serverError(w, r, "create session", err)
			return
		}
		s.setSessionCookie(w, token, expires)
	}

	writeJSON(w, r, http.StatusOK, resp)
}

type usernameRequest struct {
	Username string `json:"username"`
}

func (s *Server) handleLoginBegin(w http.ResponseWriter, r *http.Request) {
	var req usernameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "username is required")
		return
	}

	options, ceremonyID, err := s.auth.BeginLogin(r.Context(), req.Username)
	switch {
	case errors.Is(err, auth.ErrUserNotFound), errors.Is(err, auth.ErrNoCredentials), errors.Is(err, auth.ErrUserDisabled):
		// One uniform response for all three. Distinguishing them would turn
		// this endpoint into a username enumeration oracle.
		writeError(w, r, http.StatusUnauthorized, "login_failed", "could not start sign-in")
		return
	case err != nil:
		s.serverError(w, r, "begin login", err)
		return
	}

	s.setCeremonyCookie(w, ceremonyID)
	writeJSON(w, r, http.StatusOK, options)
}

func (s *Server) handleLoginFinish(w http.ResponseWriter, r *http.Request) {
	ceremonyID, ok := ceremonyIDFrom(r)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "no_ceremony", "no sign-in in progress")
		return
	}
	s.clearCookie(w, ceremonyCookie)

	user, err := s.auth.FinishLogin(r.Context(), ceremonyID, r)
	if err != nil {
		s.log.Warn("login failed", "error", err, "request_id", RequestID(r.Context()))
		writeError(w, r, http.StatusUnauthorized, "login_failed", "could not verify the passkey")
		return
	}

	token, expires, err := s.auth.NewSessionToken(r.Context(), user, auth.SessionKindWeb, r.UserAgent())
	if err != nil {
		s.serverError(w, r, "create session", err)
		return
	}
	s.setSessionCookie(w, token, expires)

	writeJSON(w, r, http.StatusOK, map[string]any{"user": userJSON(user)})
}

type recoveryRequest struct {
	Username string `json:"username"`
	Code     string `json:"code"`
}

// handleRecoveryRedeem is the lockout escape hatch reachable from a browser.
// It yields a short-lived session that can do nothing but enrol a new passkey.
func (s *Server) handleRecoveryRedeem(w http.ResponseWriter, r *http.Request) {
	var req recoveryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Code == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "username and code are required")
		return
	}

	user, err := s.auth.RedeemRecoveryCode(r.Context(), req.Username, req.Code)
	if err != nil {
		// Uniform for unknown user, disabled account and wrong code alike.
		s.log.Warn("recovery redemption failed", "error", err, "request_id", RequestID(r.Context()))
		writeError(w, r, http.StatusUnauthorized, "recovery_failed", "invalid username or recovery code")
		return
	}

	token, expires, err := s.auth.NewSessionToken(r.Context(), user, auth.SessionKindRecovery, r.UserAgent())
	if err != nil {
		s.serverError(w, r, "create recovery session", err)
		return
	}
	s.setSessionCookie(w, token, expires)

	remaining, _ := s.auth.Store().CountUnusedRecoveryCodes(r.Context(), user.ID)
	writeJSON(w, r, http.StatusOK, map[string]any{
		"user":                     userJSON(user),
		"session_kind":             auth.SessionKindRecovery,
		"remaining_recovery_codes": remaining,
		"next_step":                "Register a new passkey now — this session expires in 15 minutes and can do nothing else.",
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(r.Context(), sessionTokenFrom(r, s.cookieName)); err != nil {
		s.log.Warn("logout", "error", err)
	}
	s.clearCookie(w, s.cookieName)
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "signed out"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := CurrentUser(r.Context())
	sess := currentSession(r.Context())

	remaining, err := s.auth.Store().CountUnusedRecoveryCodes(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, "count recovery codes", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"user":                     userJSON(user),
		"session_kind":             sess.Kind,
		"remaining_recovery_codes": remaining,
	})
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := s.auth.Store().ListCredentials(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "list credentials", err)
		return
	}

	out := make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		out = append(out, map[string]any{
			"id": c.ID, "name": c.Name,
			"created_at": c.CreatedAt, "last_used_at": c.LastUsedAt,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"credentials": out})
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	user := CurrentUser(r.Context())

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid credential id")
		return
	}

	// Removing your last passkey would lock you out of your own server. Recovery
	// codes would still work, but silently walking a user into needing them is
	// a trap, not a feature.
	creds, err := s.auth.Store().ListCredentials(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, "list credentials", err)
		return
	}
	if len(creds) <= 1 {
		writeError(w, r, http.StatusConflict, "last_credential",
			"this is your only passkey; register another before removing it")
		return
	}

	if err := s.auth.Store().DeleteCredential(r.Context(), user.ID, id); err != nil {
		writeError(w, r, http.StatusNotFound, "not_found", "credential not found")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "deleted"})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.auth.Store().ListSessions(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "list sessions", err)
		return
	}

	current := currentSession(r.Context())
	out := make([]map[string]any, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, map[string]any{
			"id": sess.ID, "kind": sess.Kind, "user_agent": sess.UserAgent,
			"created_at": sess.CreatedAt, "last_seen_at": sess.LastSeenAt,
			"expires_at": sess.ExpiresAt,
			"current":    sess.ID == current.ID,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid session id")
		return
	}

	user := CurrentUser(r.Context())
	if err := s.auth.Store().RevokeSession(r.Context(), user.ID, id); err != nil {
		writeError(w, r, http.StatusNotFound, "not_found", "session not found")
		return
	}

	// Revoking your own session is a logout; clear the cookie so the browser
	// isn't left holding a token that no longer works.
	if cur := currentSession(r.Context()); cur != nil && cur.ID == id {
		s.clearCookie(w, s.cookieName)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "revoked"})
}

func (s *Server) handleRegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	user := CurrentUser(r.Context())

	codes, err := s.auth.RegenerateRecoveryCodes(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, "regenerate recovery codes", err)
		return
	}

	s.log.Warn("recovery codes regenerated", "user_id", user.ID, "username", user.Username)
	writeJSON(w, r, http.StatusOK, map[string]any{
		"recovery_codes": codes,
		"notice":         "Previous codes are now invalid. Store these; they are shown once.",
	})
}

// --- app passwords ----------------------------------------------------------

func (s *Server) handleListAppPasswords(w http.ResponseWriter, r *http.Request) {
	list, err := s.auth.Store().ListAppPasswords(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "list app passwords", err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, map[string]any{
			"id": a.ID, "name": a.Name,
			"created_at": a.CreatedAt, "last_used_at": a.LastUsedAt, "expires_at": a.ExpiresAt,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"app_passwords": out})
}

type createAppPasswordRequest struct {
	Name string `json:"name"`
	// TTLHours of 0 means it never expires.
	TTLHours int `json:"ttl_hours"`
}

func (s *Server) handleCreateAppPassword(w http.ResponseWriter, r *http.Request) {
	var req createAppPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "a name is required")
		return
	}

	// A recovery session cannot reach this handler at all: this path is absent
	// from isRecoveryAllowedPath, so requireAuth rejects it first. That matters
	// — minting a long-lived credential from a printed code would defeat the
	// 15-minute limit entirely.
	user := CurrentUser(r.Context())
	ap, plaintext, err := s.auth.Store().CreateAppPassword(
		r.Context(), user.ID, req.Name, time.Duration(req.TTLHours)*time.Hour)
	if err != nil {
		s.serverError(w, r, "create app password", err)
		return
	}

	s.log.Warn("app password created", "user", user.Username, "name", req.Name)
	writeJSON(w, r, http.StatusCreated, map[string]any{
		"app_password": map[string]any{
			"id": ap.ID, "name": ap.Name, "created_at": ap.CreatedAt, "expires_at": ap.ExpiresAt,
		},
		// Shown exactly once, like recovery codes. Only the argon2id hash is
		// stored, so there is no way to display it again.
		"password": plaintext,
		"notice":   "Copy this now — it is shown once and cannot be retrieved. Mount /dav with your username and this password.",
	})
}

func (s *Server) handleRevokeAppPassword(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid app password id")
		return
	}
	if err := s.auth.Store().RevokeAppPassword(r.Context(), CurrentUser(r.Context()).ID, id); err != nil {
		writeError(w, r, http.StatusNotFound, "not_found", "app password not found")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "revoked"})
}

// --- helpers ----------------------------------------------------------------

func userJSON(u *auth.User) map[string]any {
	return map[string]any{
		"id":           u.ID,
		"username":     u.Username,
		"display_name": u.DisplayName,
		"is_admin":     u.IsAdmin,
		"created_at":   u.CreatedAt,
	}
}

// serverError logs the real cause and returns a generic message, so internal
// details never reach the client.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.log.Error(op, "error", err, "request_id", RequestID(r.Context()))
	writeError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
}
