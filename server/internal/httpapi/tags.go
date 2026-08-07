package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Tag endpoints: read a file's tags, add and remove user tags, list a user's
// tags, and list the files under a tag. Auto tags are applied by the worker;
// these let a person curate on top of them.

func tagJSON(name, source string) map[string]any {
	return map[string]any{"name": name, "source": source}
}

func (s *Server) handleListNodeTags(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	tags, err := s.files.Store().TagsForNode(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.writeFilesError(w, r, "list tags", err)
		return
	}
	out := make([]map[string]any, 0, len(tags))
	for _, t := range tags {
		out = append(out, tagJSON(t.Name, t.Source))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"tags": out})
}

type addTagRequest struct {
	Tag string `json:"tag"`
}

func (s *Server) handleAddNodeTag(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	var req addTagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Tag == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "a tag is required")
		return
	}
	if err := s.files.Store().AddUserTag(r.Context(), CurrentUser(r.Context()).ID, id, req.Tag); err != nil {
		s.writeFilesError(w, r, "add tag", err)
		return
	}
	// Return the node's full tag set so the client does not need a follow-up read.
	tags, err := s.files.Store().TagsForNode(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.serverError(w, r, "list tags", err)
		return
	}
	out := make([]map[string]any, 0, len(tags))
	for _, t := range tags {
		out = append(out, tagJSON(t.Name, t.Source))
	}
	writeJSON(w, r, http.StatusCreated, map[string]any{"tags": out})
}

func (s *Server) handleRemoveNodeTag(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	tag := r.PathValue("tag")
	if tag == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "a tag is required")
		return
	}
	if err := s.files.Store().RemoveTag(r.Context(), CurrentUser(r.Context()).ID, id, tag); err != nil {
		s.writeFilesError(w, r, "remove tag", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "removed"})
}

func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request) {
	counts, err := s.files.Store().ListTags(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "list tags", err)
		return
	}
	out := make([]map[string]any, 0, len(counts))
	for _, c := range counts {
		out = append(out, map[string]any{"tag": c.Tag, "count": c.Count})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"tags": out})
}

func (s *Server) handleTagNodes(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	if tag == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "a tag is required")
		return
	}
	q := r.URL.Query()
	// Clamped here so has_more below compares against the limit the store
	// actually applied rather than the one the client asked for.
	limit := files.ClampSearchLimit(atoiDefault(q.Get("limit"), 0))
	offset := atoiDefault(q.Get("offset"), 0)

	nodes, err := s.files.Store().NodesByTag(r.Context(), CurrentUser(r.Context()).ID, tag, limit, offset)
	if err != nil {
		s.serverError(w, r, "nodes by tag", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"tag":      tag,
		"nodes":    nodesJSON(nodes),
		"count":    len(nodes),
		"has_more": len(nodes) == limit,
	})
}
