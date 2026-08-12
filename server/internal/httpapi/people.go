package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Phase 8: the people browser.
//
// A cluster is unnamed until a person names it. The system never guesses an
// identity — an unnamed cluster is an honest "here are faces that look alike",
// while a guessed name is a claim about a real human being that nobody made.

func personJSON(p *files.Person) map[string]any {
	out := map[string]any{
		"id":         p.ID,
		"face_count": p.FaceCount,
		"created_at": p.CreatedAt,
	}
	// Absent rather than empty when unnamed, so a client cannot mistake "nobody
	// has named this cluster" for "somebody named it the empty string".
	if p.Name != nil && *p.Name != "" {
		out["name"] = *p.Name
	}
	if p.CoverNodeID != nil {
		out["cover_node_id"] = *p.CoverNodeID
	}
	if p.CoverBox != nil {
		// Fractions, not pixels, so a client can crop from whichever variant it
		// already has.
		out["cover_box"] = []float64{p.CoverBox[0], p.CoverBox[1], p.CoverBox[2], p.CoverBox[3]}
	}
	return out
}

func faceJSON(f *files.Face) map[string]any {
	out := map[string]any{
		"id":  f.ID,
		"box": []float64{f.Box[0], f.Box[1], f.Box[2], f.Box[3]},
		"seq": f.Seq,
	}
	if f.PersonID != nil {
		out["person_id"] = *f.PersonID
	}
	return out
}

func (s *Server) writePeopleError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, files.ErrPersonNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "no such person")
	case errors.Is(err, files.ErrFaceNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "no such face")
	default:
		s.writeFilesError(w, r, op, err)
	}
}

func (s *Server) personIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid person id")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) handleListPeople(w http.ResponseWriter, r *http.Request) {
	people, err := s.files.Store().ListPeople(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "list people", err)
		return
	}
	out := make([]map[string]any, 0, len(people))
	for _, p := range people {
		out = append(out, personJSON(p))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"people": out})
}

func (s *Server) handleGetPerson(w http.ResponseWriter, r *http.Request) {
	id, ok := s.personIDParam(w, r)
	if !ok {
		return
	}
	user := CurrentUser(r.Context())

	person, err := s.files.Store().GetPerson(r.Context(), user.ID, id)
	if err != nil {
		s.writePeopleError(w, r, "get person", err)
		return
	}

	q := r.URL.Query()
	limit := files.ClampSearchLimit(atoiDefault(q.Get("limit"), 0))
	offset := atoiDefault(q.Get("offset"), 0)

	nodes, err := s.files.Store().PersonNodes(r.Context(), user.ID, id, limit, offset)
	if err != nil {
		s.writePeopleError(w, r, "person photos", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"person":   personJSON(person),
		"items":    s.nodesWithMediaJSON(r, user.ID, nodes),
		"has_more": len(nodes) == limit,
	})
}

// handlePatchPerson names a cluster, or clears the name.
func (s *Server) handlePatchPerson(w http.ResponseWriter, r *http.Request) {
	id, ok := s.personIDParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Name *string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	var name *string
	if body.Name != nil {
		trimmed := strings.TrimSpace(*body.Name)
		if trimmed != "" {
			name = &trimmed
		}
		// An empty string clears the name back to unnamed, which is how a user
		// undoes a mistaken naming without deleting the cluster.
	}

	person, err := s.files.Store().NamePerson(r.Context(), CurrentUser(r.Context()).ID, id, name)
	if err != nil {
		s.writePeopleError(w, r, "name person", err)
		return
	}
	s.audit(r, "person.name", id.String(), map[string]any{"named": name != nil})
	writeJSON(w, r, http.StatusOK, map[string]any{"person": personJSON(person)})
}

// handleMergePeople folds one cluster into another.
//
// Clustering is greedy and incremental, so one person scattered across several
// clusters is expected rather than exceptional. A faces feature with no
// correction path is one people stop trusting after the first mistake.
func (s *Server) handleMergePeople(w http.ResponseWriter, r *http.Request) {
	id, ok := s.personIDParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Into string `json:"into"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	into, err := uuid.Parse(body.Into)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid person id")
		return
	}

	if err := s.files.Store().MergePeople(r.Context(), CurrentUser(r.Context()).ID, id, into); err != nil {
		s.writePeopleError(w, r, "merge people", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "merged", "into": into})
}

// handleForgetPerson deletes a cluster. It does not touch the photographs, and
// it does not delete the detections — the faces simply become unassigned.
func (s *Server) handleForgetPerson(w http.ResponseWriter, r *http.Request) {
	id, ok := s.personIDParam(w, r)
	if !ok {
		return
	}
	if err := s.files.Store().ForgetPerson(r.Context(), CurrentUser(r.Context()).ID, id); err != nil {
		s.writePeopleError(w, r, "forget person", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "forgotten",
		"note":   "the photos and the detected faces were not deleted",
	})
}

// handleListNodeFaces answers "who is in this picture".
func (s *Server) handleListNodeFaces(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	faces, err := s.files.Store().FacesInNode(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.writePeopleError(w, r, "node faces", err)
		return
	}
	out := make([]map[string]any, 0, len(faces))
	for _, f := range faces {
		out = append(out, faceJSON(f))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"faces": out})
}

// handleReassignFace fixes one wrong detection, as opposed to merge's
// whole-cluster case. A null person_id detaches the face, which is how a user
// says "this is not a face" without deleting the detection.
func (s *Server) handleReassignFace(w http.ResponseWriter, r *http.Request) {
	faceID, err := uuid.Parse(r.PathValue("faceId"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid face id")
		return
	}
	var body struct {
		PersonID *string `json:"person_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	var personID *uuid.UUID
	if body.PersonID != nil && *body.PersonID != "" {
		parsed, err := uuid.Parse(*body.PersonID)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid person id")
			return
		}
		personID = &parsed
	}

	if err := s.files.Store().ReassignFace(r.Context(), CurrentUser(r.Context()).ID, faceID, personID); err != nil {
		s.writePeopleError(w, r, "reassign face", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "reassigned"})
}
