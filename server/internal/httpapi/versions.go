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
