package httpapi

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/shares"
)

// shareCookie carries the unlock proof for one share. It is path-scoped to that
// share's endpoints, so a proof for one link is never sent to another, and it is
// HttpOnly so page scripts cannot read it.
const shareCookie = "pc_share"

// writeShareError maps the share plane's deliberately coarse errors onto status
// codes. The public surface never distinguishes "revoked" from "never existed":
// both are 404, so a probe cannot confirm a token ever named anything.
func (s *Server) writeShareError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, shares.ErrNotFound), errors.Is(err, files.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "no such share")
	case errors.Is(err, shares.ErrGone):
		writeError(w, r, http.StatusGone, "gone", "this link is no longer available")
	case errors.Is(err, shares.ErrPasswordNeeded):
		writeError(w, r, http.StatusUnauthorized, "password_required", "this link needs a password")
	case errors.Is(err, shares.ErrWrongPassword):
		writeError(w, r, http.StatusUnauthorized, "wrong_password", "incorrect password")
	case errors.Is(err, shares.ErrNotAFile):
		writeError(w, r, http.StatusBadRequest, "not_a_file", "that path is not a file")
	case errors.Is(err, shares.ErrShareTargetKind):
		writeError(w, r, http.StatusBadRequest, "cannot_share", "only files and folders can be shared")
	default:
		s.serverError(w, r, op, err)
	}
}

// --- management (authenticated) ---------------------------------------------

type createShareRequest struct {
	Password       string  `json:"password"`
	ExpiresInHours float64 `json:"expires_in_hours"`
	MaxDownloads   int64   `json:"max_downloads"`
}

func (s *Server) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	// An empty body means "a plain link": no password, no expiry, no cap.
	var req createShareRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "expected a JSON body")
			return
		}
	}

	share, token, err := s.shares.Create(r.Context(), CurrentUser(r.Context()).ID, id, shares.CreateOptions{
		Password:     req.Password,
		ExpiresIn:    time.Duration(req.ExpiresInHours * float64(time.Hour)),
		MaxDownloads: req.MaxDownloads,
	})
	if err != nil {
		s.writeShareError(w, r, "create share", err)
		return
	}

	out := map[string]any{
		"id":           share.ID,
		"has_password": share.HasPassword(),
		// The token is returned exactly once — it is never stored in plaintext, so
		// it can never be shown again. The client must surface it now.
		"token": token,
		"path":  "/s/" + token,
	}
	if share.ExpiresAt != nil {
		out["expires_at"] = *share.ExpiresAt
	}
	if share.MaxDownloads != nil {
		out["max_downloads"] = *share.MaxDownloads
	}
	writeJSON(w, r, http.StatusCreated, map[string]any{"share": out})
}

// ownerShareJSON is the owner's view of their own link. It may show the file
// name and path — the viewer owns them — but never the token, which is not
// stored and cannot be reconstructed.
func ownerShareJSON(os shares.OwnerShare) map[string]any {
	now := time.Now()
	m := map[string]any{
		"id":             os.ID,
		"file_name":      os.NodeName,
		"path":           os.NodePath,
		"created_at":     os.CreatedAt,
		"has_password":   os.HasPassword(),
		"download_count": os.DownloadCount,
		"revoked":        os.Revoked(),
		"expired":        os.Expired(now),
		"file_trashed":   os.NodeTrashed,
		"active":         os.Active(now) && !os.NodeTrashed,
	}
	if os.ExpiresAt != nil {
		m["expires_at"] = *os.ExpiresAt
	}
	if os.MaxDownloads != nil {
		m["max_downloads"] = *os.MaxDownloads
	}
	if os.RevokedAt != nil {
		m["revoked_at"] = *os.RevokedAt
	}
	return m
}

func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request) {
	list, err := s.shares.List(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "list shares", err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, os := range list {
		out = append(out, ownerShareJSON(os))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"shares": out})
}

func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid share id")
		return
	}
	ok, err := s.shares.Revoke(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.serverError(w, r, "revoke share", err)
		return
	}
	if !ok {
		writeError(w, r, http.StatusNotFound, "not_found", "no such share, or already revoked")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "revoked"})
}

// --- public plane (unauthenticated) -----------------------------------------

func (s *Server) handleShareView(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	view, err := s.shares.View(r.Context(), token, s.readUnlockProof(r), r.URL.Query().Get("path"))
	if err != nil {
		s.writeShareError(w, r, "view share", err)
		return
	}
	writeJSON(w, r, http.StatusOK, shareViewJSON(view))
}

type unlockRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleShareUnlock(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	var req unlockRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "expected a JSON body")
			return
		}
	}
	proof, err := s.shares.Unlock(r.Context(), token, req.Password)
	if err != nil {
		s.writeShareError(w, r, "unlock share", err)
		return
	}
	s.setUnlockCookie(w, token, proof)
	writeJSON(w, r, http.StatusOK, map[string]any{"unlocked": true})
}

func (s *Server) handleShareContent(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	content, rc, err := s.shares.OpenContent(r.Context(), token, s.readUnlockProof(r), r.URL.Query().Get("path"))
	if err != nil {
		s.writeShareError(w, r, "open share content", err)
		return
	}
	defer rc.Close()

	if len(content.ContentHash) > 0 {
		w.Header().Set("ETag", `"`+hex.EncodeToString(content.ContentHash)+`"`)
	}
	w.Header().Set("Content-Type", content.MIME)
	// public content, but still sandboxed: a shared HTML or SVG must not execute
	// with any privilege, and on the public host there is no session cookie to
	// steal — but nosniff + sandbox keep it inert regardless.
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Disposition", contentDisposition(content.Name, r.URL.Query().Get("download") != ""))
	w.Header().Set("Accept-Ranges", "bytes")

	counter := &countingWriter{ResponseWriter: w}
	http.ServeContent(counter, r, content.Name, content.ModTime, rc)
	s.metrics.DownloadBytes.Add(float64(counter.n))
}

// --- unlock cookie ----------------------------------------------------------

// setUnlockCookie scopes the proof to this share's path, so it is never sent to
// any other share or anywhere else in the API.
func (s *Server) setUnlockCookie(w http.ResponseWriter, token, proof string) {
	http.SetCookie(w, &http.Cookie{
		Name:     shareCookie,
		Value:    proof,
		Path:     "/api/v1/s/" + token,
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) readUnlockProof(r *http.Request) string {
	c, err := r.Cookie(shareCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func shareViewJSON(v *shares.PublicShare) map[string]any {
	out := map[string]any{
		"has_password": v.HasPassword,
		"unlocked":     v.Unlocked,
	}
	if !v.Unlocked {
		return out
	}
	out["name"] = v.Name
	out["kind"] = v.Kind
	out["path"] = v.Path
	if v.Kind == "file" {
		out["size"] = v.Size
		out["mime"] = v.MIME
		return out
	}
	entries := make([]map[string]any, 0, len(v.Entries))
	for _, e := range v.Entries {
		item := map[string]any{"name": e.Name, "kind": e.Kind}
		if e.Kind == "file" {
			item["size"] = e.Size
		}
		entries = append(entries, item)
	}
	out["entries"] = entries
	return out
}
