package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/media"
)

// Phase 5: media metadata, derived variants and the photo timeline.

// mediaJSON renders a node's media metadata for the API.
//
// Every field is omitted when absent rather than sent as null. A PNG has no
// capture time and a photo has no duration, and a client that has to distinguish
// "absent" from "zero" for width would have to guess.
func mediaJSON(m *files.MediaMeta) map[string]any {
	out := map[string]any{"orientation": m.Orientation}
	if m.Width > 0 {
		out["width"] = m.Width
	}
	if m.Height > 0 {
		out["height"] = m.Height
	}
	// Deliberately omitted when unknown rather than defaulted to the upload time.
	// The contract makes the fallback to updated_at the CLIENT's job precisely so
	// it stays visible as a fallback — a UI can say "date unknown" instead of
	// presenting an import date as the moment the shutter fired.
	if m.TakenAt != nil {
		out["taken_at"] = *m.TakenAt
	}
	if m.Camera != "" {
		out["camera"] = m.Camera
	}
	if m.GPSLat != nil && m.GPSLon != nil {
		out["gps"] = map[string]any{"lat": *m.GPSLat, "lon": *m.GPSLon}
	}
	if m.DurationMS != nil {
		out["duration_ms"] = *m.DurationMS
	}
	if m.Source != "" {
		out["source"] = m.Source
	}
	// Always present, possibly empty: a gallery reads this to decide whether to
	// request a thumbnail or fall back to the original, and an absent key would
	// make "none yet" indistinguishable from "this server has no variants at
	// all". An empty array answers both.
	variants := m.Variants
	if variants == nil {
		variants = []string{}
	}
	out["variants"] = variants
	return out
}

// serveMediaVariant streams a rendered thumbnail or preview.
//
// A variant that has not been rendered yet is an honest 404 rather than a silent
// fallback to the original: a grid of 200 tiles quietly served full-size
// originals would pull gigabytes and look like a slow network instead of a
// missing job.
func (s *Server) serveMediaVariant(w http.ResponseWriter, r *http.Request, id uuid.UUID, variant string) {
	if variant != media.VariantThumb && variant != media.VariantPreview {
		writeError(w, r, http.StatusBadRequest, "invalid_variant",
			"variant must be thumb, preview or original")
		return
	}

	v, content, err := s.files.OpenMediaVariant(r.Context(), CurrentUser(r.Context()).ID, id, variant)
	if errors.Is(err, files.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "variant_unavailable",
			"that rendition has not been generated for this file yet")
		return
	}
	if err != nil {
		s.writeFilesError(w, r, "open media variant", err)
		return
	}
	defer content.Close()

	// The variant's own identity, not the original's. Two files with different
	// originals can share a thumbnail only if the thumbnails are byte-identical,
	// in which case they legitimately share a cache entry.
	w.Header().Set("ETag", `"`+variant+"-"+v.StorageKey+`"`)
	w.Header().Set("Content-Type", v.MIME)
	// Immutable: a variant is derived from content-addressed bytes, so this exact
	// rendition never changes. That is what makes a gallery cheap on a second
	// visit — and it is safe in a way the original's revalidate-always policy is
	// not, because there is no version history to fall out of date with.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Accept-Ranges", "bytes")

	counter := &countingWriter{ResponseWriter: w}
	http.ServeContent(counter, r, "", time.Time{}, content)
	s.metrics.DownloadBytes.Add(float64(counter.n))
}

// handleTimeline serves the gallery's primary read: media newest first by
// capture time.
//
// Kept off /search because it sorts by when a photo was taken and pages by date
// rather than by relevance. Metadata rides along per item so a grid of tiles
// does not need a request each to learn whether a thumbnail exists.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	from, ok := s.timeParam(w, r, q.Get("from"), "from")
	if !ok {
		return
	}
	to, ok := s.timeParam(w, r, q.Get("to"), "to")
	if !ok {
		return
	}

	limit := files.ClampSearchLimit(atoiDefault(q.Get("limit"), 0))
	offset := atoiDefault(q.Get("offset"), 0)
	user := CurrentUser(r.Context())

	nodes, err := s.files.Store().TimelineNodes(r.Context(), user.ID, from, to, limit, offset)
	if err != nil {
		s.serverError(w, r, "media timeline", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"items":    s.nodesWithMediaJSON(r, user.ID, nodes),
		"count":    len(nodes),
		"has_more": len(nodes) == limit,
	})
}

// nodesWithMediaJSON renders nodes with their media metadata attached, in one
// extra query rather than one per node.
//
// Best effort on the metadata: a failure to load it degrades the grid to
// originals, which is worse-looking but correct, and is a far better outcome
// than failing the whole listing.
func (s *Server) nodesWithMediaJSON(r *http.Request, ownerID uuid.UUID, nodes []*files.Node) []map[string]any {
	out := nodesJSON(nodes)

	ids := make([]uuid.UUID, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	metas, err := s.files.Store().MediaMetaForNodes(r.Context(), ownerID, ids)
	if err != nil {
		s.log.Warn("media metadata for listing failed", "error", err,
			"request_id", RequestID(r.Context()))
		return out
	}
	for i, n := range nodes {
		if m, ok := metas[n.ID]; ok {
			out[i]["media"] = mediaJSON(m)
		}
	}
	return out
}

// timeParam parses an optional RFC 3339 bound.
func (s *Server) timeParam(w http.ResponseWriter, r *http.Request, raw, name string) (*time.Time, bool) {
	if raw == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_time",
			name+" must be an RFC 3339 timestamp")
		return nil, false
	}
	return &t, true
}
