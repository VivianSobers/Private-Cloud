package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Phase 7: the admin console.
//
// cloudctl on the server remains the break-glass path and is strictly more
// powerful — it needs shell access, which already implies database and file
// access, so it weakens nothing. These endpoints exist so routine account work
// does not require an SSH session. Every one is admin-only server-side; the
// client's nav gating is convenience, never the boundary.

func adminUserJSON(u *auth.User) map[string]any {
	out := map[string]any{
		"id":           u.ID,
		"username":     u.Username,
		"display_name": u.DisplayName,
		"is_admin":     u.IsAdmin,
		"disabled":     u.Disabled(),
		"created_at":   u.CreatedAt,
	}
	// Absent means unlimited. Sending 0 would be a quota of zero bytes, which is
	// the opposite of what a missing quota means.
	if u.QuotaBytes != nil {
		out["quota_bytes"] = *u.QuotaBytes
	}
	if u.DisabledAt != nil {
		out["disabled_at"] = *u.DisabledAt
	}
	return out
}

func (s *Server) writeAdminError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "no such user")
	case errors.Is(err, auth.ErrLastAdmin):
		// 409, not 403: the caller has the right to do this in general, and the
		// server is refusing this instance of it because of the current state.
		writeError(w, r, http.StatusConflict, "last_admin",
			"this is the only administrator left; promote another account first")
	case errors.Is(err, auth.ErrUserExists):
		writeError(w, r, http.StatusConflict, "username_taken", "that username is already in use")
	default:
		s.serverError(w, r, op, err)
	}
}

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.auth.Store().ListUsers(r.Context())
	if err != nil {
		s.serverError(w, r, "list users", err)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		entry := adminUserJSON(u)
		// Usage per account, so a quota column means something. Best effort: a
		// failed usage read leaves the numbers absent rather than failing the
		// whole list.
		if usage, err := s.files.Store().Usage(r.Context(), u.ID); err == nil {
			// used_bytes is TotalBytes — the same figure the quota is measured
			// against, and the same one /usage reports. It was LiveBytes, so an
			// admin watched an account refused at its 4 GiB limit while the column
			// beside the limit read 2 GiB, with nothing on the screen to explain the
			// gap. A number labelled "used" that is not the number enforcement uses
			// is worse than no number.
			entry["used_bytes"] = usage.TotalBytes()
			// The parts, so the total is explicable rather than merely correct. An
			// admin looking at a full account needs to know whether the answer is
			// "empty the trash", "wait for retention" or "buy a disk".
			entry["live_bytes"] = usage.LiveBytes
			entry["trash_bytes"] = usage.TrashBytes
			entry["version_bytes"] = usage.VersionBytes
			entry["file_count"] = usage.FileCount
		}
		out = append(out, entry)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"users": out})
}

// handleAdminCreateUser provisions an account.
//
// It returns recovery codes exactly once, because a new account has no passkey
// yet and no way to enrol one without first signing in. That is the same
// bootstrap `cloudctl user create` performs, and the codes are shown once and
// stored argon2id-hashed — this response is the only time they exist in plain
// text.
func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		IsAdmin     bool   `json:"is_admin"`
		QuotaBytes  *int64 `json:"quota_bytes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "a username is required")
		return
	}
	if body.DisplayName == "" {
		body.DisplayName = body.Username
	}

	// The id is minted here rather than by the database because the bootstrap
	// registration path needs the same UUID to exist before the row does — it
	// becomes the WebAuthn user handle. Same rule as `cloudctl user create`.
	user, err := s.auth.Store().CreateUser(r.Context(), uuid.New(),
		body.Username, body.DisplayName, body.IsAdmin)
	if err != nil {
		s.writeAdminError(w, r, "create user", err)
		return
	}

	// The new account has no passkey yet, and recovery codes are how it gets in
	// the first time: redeem one, then enrol a passkey. That reuses the recovery
	// path rather than inventing a separate invite-token concept.
	codes, err := s.auth.RegenerateRecoveryCodes(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, "generate recovery codes", err)
		return
	}
	if body.QuotaBytes != nil {
		quota := body.QuotaBytes
		if _, err := s.auth.Store().UpdateUser(r.Context(), user.ID,
			auth.UserPatch{QuotaBytes: &quota}); err != nil {
			s.writeAdminError(w, r, "set quota", err)
			return
		}
		user.QuotaBytes = body.QuotaBytes
	}

	s.audit(r, "user.create", user.Username, map[string]any{"is_admin": user.IsAdmin})
	writeJSON(w, r, http.StatusCreated, map[string]any{
		"user":           adminUserJSON(user),
		"recovery_codes": codes,
	})
}

func (s *Server) handleAdminPatchUser(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid user id")
		return
	}

	var body struct {
		DisplayName *string `json:"display_name"`
		IsAdmin     *bool   `json:"is_admin"`
		Disabled    *bool   `json:"disabled"`
		// Present-and-null clears the quota; absent leaves it alone. json.Decode
		// cannot distinguish those on a **int64, so the raw presence is read from
		// a map below.
		QuotaBytes *int64 `json:"quota_bytes"`
	}
	raw := map[string]any{}
	if !decodeJSONInto(w, r, &body, &raw) {
		return
	}

	patch := auth.UserPatch{
		DisplayName: body.DisplayName,
		IsAdmin:     body.IsAdmin,
		Disabled:    body.Disabled,
	}
	if _, present := raw["quota_bytes"]; present {
		quota := body.QuotaBytes
		patch.QuotaBytes = &quota
	}

	user, err := s.auth.Store().UpdateUser(r.Context(), id, patch)
	if err != nil {
		s.writeAdminError(w, r, "update user", err)
		return
	}
	s.audit(r, "user.update", user.Username, map[string]any{
		"is_admin": user.IsAdmin, "disabled": user.Disabled(),
	})
	writeJSON(w, r, http.StatusOK, map[string]any{"user": adminUserJSON(user)})
}

// handleAdminDeleteUser disables and revokes. It does NOT delete.
//
// Deleting a user cascades their files away, and "remove this person's access"
// almost never means "destroy everything they ever uploaded". Making that
// irreversible step one console button away is how it happens by accident; the
// path to it is a shell, a backup check and cloudctl.
func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid user id")
		return
	}
	if err := s.auth.Store().DisableUser(r.Context(), id); err != nil {
		s.writeAdminError(w, r, "disable user", err)
		return
	}
	s.audit(r, "user.disable", id.String(), nil)
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "disabled",
		"note":   "the account is disabled and its sessions revoked; its files were not deleted",
	})
}

func (s *Server) handleAdminUserSessions(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid user id")
		return
	}
	sessions, err := s.auth.Store().ListSessions(r.Context(), id)
	if err != nil {
		s.serverError(w, r, "list user sessions", err)
		return
	}
	out := make([]map[string]any, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, map[string]any{
			"id":           sess.ID,
			"kind":         sess.Kind,
			"user_agent":   sess.UserAgent,
			"created_at":   sess.CreatedAt,
			"last_seen_at": sess.LastSeenAt,
			"expires_at":   sess.ExpiresAt,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) handleAdminRevokeUserSession(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid user id")
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("sid"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid session id")
		return
	}
	if err := s.auth.Store().RevokeSession(r.Context(), userID, sessionID); err != nil {
		writeError(w, r, http.StatusNotFound, "not_found", "no such session")
		return
	}
	s.audit(r, "session.revoke", userID.String(), map[string]any{"session": sessionID.String()})
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "revoked"})
}

// handleAdminAudit reads the authorisation log.
func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	from, ok := s.timeParam(w, r, q.Get("from"), "from")
	if !ok {
		return
	}
	to, ok := s.timeParam(w, r, q.Get("to"), "to")
	if !ok {
		return
	}

	filter := files.AuditFilter{
		Actor:  strings.TrimSpace(q.Get("actor")),
		Action: strings.TrimSpace(q.Get("action")),
		From:   from,
		To:     to,
		Limit:  files.ClampSearchLimit(atoiDefault(q.Get("limit"), 0)),
		Offset: atoiDefault(q.Get("offset"), 0),
	}

	entries, err := s.files.Store().ListAudit(r.Context(), filter)
	if err != nil {
		s.serverError(w, r, "list audit", err)
		return
	}

	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		entry := map[string]any{
			"id":     e.ID,
			"at":     e.At.UTC().Format(time.RFC3339),
			"actor":  e.Actor,
			"action": e.Action,
			"target": e.Target,
		}
		if e.RequestID != "" {
			entry["request_id"] = e.RequestID
		}
		if len(e.Detail) > 0 {
			entry["detail"] = e.Detail
		}
		out = append(out, entry)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"entries":  out,
		"has_more": len(out) == filter.Limit,
	})
}
