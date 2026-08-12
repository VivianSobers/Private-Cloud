package httpapi

import (
	"errors"
	"net/http"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Phase 8: "more like this".
//
// Reuses the embedding space Phase 4 built — no new job kind, no new model, no
// new table. It degrades exactly the way semantic search already does, because a
// client that already handles 503 from /search needs no new failure path.

func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	if s.embedder == nil {
		writeError(w, r, http.StatusServiceUnavailable, "semantic_unavailable",
			"similarity is not enabled on this server")
		return
	}

	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	user := CurrentUser(r.Context())
	shared := includeShared(r)
	limit := files.ClampSearchLimit(atoiDefault(r.URL.Query().Get("limit"), 10))

	results, err := s.files.Store().SimilarTo(r.Context(), user.ID, id,
		s.embedder.Model(), limit, shared)
	if errors.Is(err, files.ErrNoEmbedding) {
		// 404, not 503: the server is fine and the feature is enabled — this
		// particular file simply has nothing to compare, because it has no
		// extractable text or has not been indexed yet. A 503 would tell the
		// client to retry the whole feature, which would never help.
		writeError(w, r, http.StatusNotFound, "not_indexed",
			"this file has not been indexed for similarity")
		return
	}
	if err != nil {
		s.writeFilesError(w, r, "similar files", err)
		return
	}

	out := make([]map[string]any, 0, len(results))
	nodes := make([]*files.Node, 0, len(results))
	for _, res := range results {
		item := nodeJSON(res.Node)
		item["score"] = res.Score
		out = append(out, item)
		nodes = append(nodes, res.Node)
	}
	if shared {
		out = s.attachAccess(r, user.ID, nodes, out)
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"results": out,
		"count":   len(out),
	})
}
