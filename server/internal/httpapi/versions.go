package httpapi

import (
	"encoding/hex"
	"net/http"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// versionJSON is the wire shape for one entry of a file's history.
//
// The version id IS exposed here, unlike in nodeJSON: restoring and downloading
// a past version need to name it, and a version id addresses immutable content,
// not the on-disk layout nodeJSON hides. The hash key names its algorithm so a
// downloaded version stays verifiable, exactly as for a live node.
func versionJSON(v files.Version) map[string]any {
	out := map[string]any{
		"id":         v.ID,
		"size":       v.Size,
		"mime":       v.MIME,
		"created_at": v.CreatedAt,
		"is_head":    v.IsHead,
	}
	if v.CreatedBy != nil {
		out["created_by"] = *v.CreatedBy
	}
	if len(v.ContentHash) > 0 {
		if v.ManifestID != nil {
			out["blake3"] = hex.EncodeToString(v.ContentHash)
		} else {
			out["sha256"] = hex.EncodeToString(v.ContentHash)
		}
	}
	return out
}

// versionIDParam resolves the {versionId} path segment.
func (s *Server) versionIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("versionId"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid version id")
		return uuid.Nil, false
	}
	return id, true
}

// handleListVersions returns a file's history, newest first.
func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	versions, err := s.files.Store().ListVersions(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.writeFilesError(w, r, "list versions", err)
		return
	}
	out := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		out = append(out, versionJSON(v))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"versions": out})
}

// handleRestoreVersion rolls a file back to a past version by appending a new
// head with that content — the history in between is preserved, so the rollback
// is itself undoable.
func (s *Server) handleRestoreVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	versionID, ok := s.versionIDParam(w, r)
	if !ok {
		return
	}
	node, err := s.files.Store().RestoreVersion(r.Context(), CurrentUser(r.Context()).ID, id, versionID)
	if err != nil {
		s.writeFilesError(w, r, "restore version", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"node": nodeJSON(node)})
}

// handleDownloadVersion serves one past version's bytes. It carries the same
// sandboxing headers as the live download — a stored HTML or SVG version is just
// as capable of same-origin XSS as the current one — and leans on ServeContent
// for Range, If-Range and 304 handling. The modtime is the version's own
// created_at, so caches key on the snapshot rather than the file's latest edit.
func (s *Server) handleDownloadVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	versionID, ok := s.versionIDParam(w, r)
	if !ok {
		return
	}

	vc, content, err := s.files.OpenVersion(r.Context(), CurrentUser(r.Context()).ID, id, versionID)
	if err != nil {
		s.writeFilesError(w, r, "open version", err)
		return
	}
	defer content.Close()

	if len(vc.ContentHash) > 0 {
		w.Header().Set("ETag", `"`+hex.EncodeToString(vc.ContentHash)+`"`)
	}
	w.Header().Set("Content-Type", vc.MIME)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Disposition", contentDisposition(vc.Name, r.URL.Query().Get("download") != ""))
	w.Header().Set("Accept-Ranges", "bytes")

	counter := &countingWriter{ResponseWriter: w}
	http.ServeContent(counter, r, vc.Name, vc.CreatedAt, content)
	s.metrics.DownloadBytes.Add(float64(counter.n))
}
