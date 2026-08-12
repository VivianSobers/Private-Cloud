package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Phase 5: albums — hand-ordered collections that move nothing on disk.

func albumJSON(a *files.Album) map[string]any {
	out := map[string]any{
		"id":          a.ID,
		"name":        a.Name,
		"description": a.Description,
		"item_count":  a.ItemCount,
		"created_at":  a.CreatedAt,
		"updated_at":  a.UpdatedAt,
	}
	if a.CoverNodeID != nil {
		out["cover_node_id"] = *a.CoverNodeID
	}
	return out
}

// writeAlbumError maps album-domain errors in one place, alongside the file
// errors writeFilesError already handles.
func (s *Server) writeAlbumError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, files.ErrAlbumNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "no such album")
	case errors.Is(err, files.ErrInvalidAlbumName):
		writeError(w, r, http.StatusBadRequest, "invalid_name", "an album needs a name")
	case errors.Is(err, files.ErrTooManyAlbumItems):
		writeError(w, r, http.StatusRequestEntityTooLarge, "too_many_items",
			"too many items in one request")
	default:
		s.writeFilesError(w, r, op, err)
	}
}

func (s *Server) albumIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid album id")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) handleListAlbums(w http.ResponseWriter, r *http.Request) {
	albums, err := s.files.Store().ListAlbums(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "list albums", err)
		return
	}
	out := make([]map[string]any, 0, len(albums))
	for _, a := range albums {
		out = append(out, albumJSON(a))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"albums": out})
}

func (s *Server) handleCreateAlbum(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	album, err := s.files.Store().CreateAlbum(r.Context(),
		CurrentUser(r.Context()).ID, body.Name, body.Description)
	if err != nil {
		s.writeAlbumError(w, r, "create album", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, map[string]any{"album": albumJSON(album)})
}

// handleGetAlbum returns the album and a page of its items, in the user's order.
func (s *Server) handleGetAlbum(w http.ResponseWriter, r *http.Request) {
	id, ok := s.albumIDParam(w, r)
	if !ok {
		return
	}
	user := CurrentUser(r.Context())

	album, err := s.files.Store().GetAlbum(r.Context(), user.ID, id)
	if err != nil {
		s.writeAlbumError(w, r, "get album", err)
		return
	}

	q := r.URL.Query()
	limit := files.ClampSearchLimit(atoiDefault(q.Get("limit"), 0))
	offset := atoiDefault(q.Get("offset"), 0)

	items, err := s.files.Store().AlbumItems(r.Context(), user.ID, id, limit, offset)
	if err != nil {
		s.writeAlbumError(w, r, "album items", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"album":    albumJSON(album),
		"items":    s.nodesWithMediaJSON(r, user.ID, items),
		"has_more": len(items) == limit,
	})
}

func (s *Server) handlePatchAlbum(w http.ResponseWriter, r *http.Request) {
	id, ok := s.albumIDParam(w, r)
	if !ok {
		return
	}

	// Pointers throughout: this endpoint has to distinguish "field absent" from
	// "field set to empty", and for the cover "leave it" from "clear it". A
	// struct of plain values cannot express that difference.
	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		CoverNodeID *string `json:"cover_node_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	patch := files.AlbumPatch{Name: body.Name, Description: body.Description}
	if body.CoverNodeID != nil {
		var cover *uuid.UUID
		if *body.CoverNodeID != "" {
			parsed, err := uuid.Parse(*body.CoverNodeID)
			if err != nil {
				writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid node id")
				return
			}
			cover = &parsed
		}
		// An explicit empty string clears the cover; a node id sets it.
		patch.CoverNodeID = &cover
	}

	album, err := s.files.Store().UpdateAlbum(r.Context(), CurrentUser(r.Context()).ID, id, patch)
	if err != nil {
		s.writeAlbumError(w, r, "update album", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"album": albumJSON(album)})
}

func (s *Server) handleDeleteAlbum(w http.ResponseWriter, r *http.Request) {
	id, ok := s.albumIDParam(w, r)
	if !ok {
		return
	}
	if err := s.files.Store().DeleteAlbum(r.Context(), CurrentUser(r.Context()).ID, id); err != nil {
		s.writeAlbumError(w, r, "delete album", err)
		return
	}
	// Worth saying out loud in the response: deleting an album never deletes the
	// photos in it.
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "deleted",
		"note":   "the files in this album were not deleted",
	})
}

func (s *Server) handleAddAlbumItems(w http.ResponseWriter, r *http.Request) {
	id, ok := s.albumIDParam(w, r)
	if !ok {
		return
	}
	nodeIDs, ok := s.albumNodeIDs(w, r)
	if !ok {
		return
	}

	added, err := s.files.Store().AddAlbumItems(r.Context(), CurrentUser(r.Context()).ID, id, nodeIDs)
	if err != nil {
		s.writeAlbumError(w, r, "add album items", err)
		return
	}
	// `added` can be lower than what was asked for: ids that are not the
	// caller's own live files are skipped, and re-adding an existing item is a
	// no-op. Reporting the number rather than failing is what makes a retried
	// request safe.
	writeJSON(w, r, http.StatusCreated, map[string]any{
		"status": "ok",
		"added":  added,
	})
}

func (s *Server) handleRemoveAlbumItem(w http.ResponseWriter, r *http.Request) {
	id, ok := s.albumIDParam(w, r)
	if !ok {
		return
	}
	nodeID, err := uuid.Parse(r.PathValue("nodeId"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid node id")
		return
	}

	if err := s.files.Store().RemoveAlbumItem(r.Context(), CurrentUser(r.Context()).ID, id, nodeID); err != nil {
		s.writeAlbumError(w, r, "remove album item", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "removed",
		"note":   "the file itself was not deleted",
	})
}

// handleReorderAlbum replaces the entire order in one call.
func (s *Server) handleReorderAlbum(w http.ResponseWriter, r *http.Request) {
	id, ok := s.albumIDParam(w, r)
	if !ok {
		return
	}
	nodeIDs, ok := s.albumNodeIDs(w, r)
	if !ok {
		return
	}
	user := CurrentUser(r.Context())

	if err := s.files.Store().ReorderAlbum(r.Context(), user.ID, id, nodeIDs); err != nil {
		s.writeAlbumError(w, r, "reorder album", err)
		return
	}
	album, err := s.files.Store().GetAlbum(r.Context(), user.ID, id)
	if err != nil {
		s.writeAlbumError(w, r, "get album", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"album": albumJSON(album)})
}

// albumNodeIDs decodes and validates the {node_ids: [...]} body both the add and
// the reorder endpoints take.
func (s *Server) albumNodeIDs(w http.ResponseWriter, r *http.Request) ([]uuid.UUID, bool) {
	var body struct {
		NodeIDs []string `json:"node_ids"`
	}
	if !decodeJSON(w, r, &body) {
		return nil, false
	}
	if len(body.NodeIDs) > files.MaxAlbumItemsPerRequest {
		writeError(w, r, http.StatusRequestEntityTooLarge, "too_many_items",
			"too many items in one request")
		return nil, false
	}

	out := make([]uuid.UUID, 0, len(body.NodeIDs))
	for _, raw := range body.NodeIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			// Rejected rather than skipped: a malformed id in a drag-reorder means
			// the client and the server disagree about the order, and silently
			// dropping it would apply an order nobody asked for.
			writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid node id: "+raw)
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

// decodeJSON reads a JSON body, reporting a parse failure in the standard shape.
// The body is already size-capped by withBodyLimit.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "could not parse the request body")
		return false
	}
	return true
}

// decodeJSONInto decodes a body twice: once into a typed struct, and once into a
// map so the caller can tell "field absent" from "field present and null".
//
// Unmarshalling into a *T cannot express that difference — both leave the
// pointer nil — and for a nullable column like quota_bytes the two are opposite
// instructions: leave it alone, versus clear it. Reading the raw map is the
// cheapest way to recover the distinction without a custom UnmarshalJSON on
// every such field.
func decodeJSONInto(w http.ResponseWriter, r *http.Request, dst any, raw *map[string]any) bool {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "could not read the request body")
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "could not parse the request body")
		return false
	}
	if err := json.Unmarshal(body, raw); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "could not parse the request body")
		return false
	}
	return true
}
