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
//
// Slice 5 added the image space alongside it. The routing decision is NOT made
// here: it is made in the store, after the access check, because deciding "is
// this node an image the server has indexed?" before knowing whether the caller
// may read the node at all would reopen the probe that `/similar` requires read
// on its source to close. This handler's whole contribution is naming the spaces
// this server has configured.

func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	// Either space is enough. A deployment running only the image sidecar can
	// still answer "photos like this one", and one running only the text
	// embedder behaves exactly as it did before slice 5.
	if s.embedder == nil && s.imageEmbedder == nil {
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

	// The models, not the clients: ranking reads vectors already in the table and
	// never calls a sidecar, so /similar cannot hang on one and has no failure
	// path of its own to add. What the API needs from the configuration is only
	// the identity of each space.
	spaces := files.SimilarSpaces{}
	if s.embedder != nil {
		spaces.Text = s.embedder.Model()
	}
	if s.imageEmbedder != nil {
		spaces.Image = s.imageEmbedder.Model()
	}

	results, space, err := s.files.Store().SimilarTo(r.Context(), user.ID, id, spaces, limit, shared)
	if errors.Is(err, files.ErrNoEmbedding) {
		// 404, not 503: the server is fine and the feature is enabled — this
		// particular file simply has nothing to compare, because it has no
		// extractable text, no image vector, or has not been indexed yet. A 503
		// would tell the client to retry the whole feature, which would never
		// help.
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
		// Which space ranked these. On the envelope rather than on each result,
		// because the ranking happened in exactly one space and a per-result
		// field would imply the list could mix two — scores from different
		// spaces are not comparable, and a client that sorted them together
		// would be sorting noise.
		"space": space,
	})
}
