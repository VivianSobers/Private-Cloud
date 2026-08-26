package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Phase 8, open item 9: feedback controls that feed labels back.
//
// The item is easy to read as "collect labels and retrain", and that reading is
// out of scope twice over: Phase 4 ruled model training out, and a face
// correction is already permanent (faces.dismissed_at, migration 00024) without
// any model being involved. So this is the version that is in scope and is
// actually worth building — a person's judgement on a machine-produced result,
// recorded durably, readable back by them, and CONSULTED BY RETRIEVAL the next
// time it answers them. A "wrong" verdict on a neighbour or a search hit removes
// it from that person's later results (files.NotMarkedWrong, spliced into the
// vector scan beside the ACL filter). Nothing is retrained; the effect is in the
// query.
//
// The access rule is the phase's, unchanged: you may only judge a result you
// could already read, and "you may not read this" and "there is no such thing"
// are the same answer. A feedback endpoint that answered 403 for a node the
// caller cannot see would turn every judgement into an existence oracle — the
// same probe /similar's read check on the source exists to close.

func feedbackJSON(f *files.Feedback) map[string]any {
	out := map[string]any{
		"id":         f.ID,
		"kind":       f.Kind,
		"verdict":    f.Verdict,
		"created_at": f.CreatedAt,
		"updated_at": f.UpdatedAt,
	}
	// Absent rather than empty, the way personJSON handles an unnamed cluster: a
	// client must not have to tell "no note" apart from "a note that says
	// nothing".
	if f.NodeID != nil {
		out["node_id"] = *f.NodeID
	}
	if f.PersonID != nil {
		out["person_id"] = *f.PersonID
	}
	if f.Context != "" {
		out["context"] = f.Context
	}
	if f.Note != "" {
		out["note"] = f.Note
	}
	if f.Path != "" {
		out["path"] = f.Path
	}
	if f.Name != "" {
		out["name"] = f.Name
	}
	return out
}

func (s *Server) writeFeedbackError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, files.ErrInvalidFeedback):
		writeError(w, r, http.StatusBadRequest, "invalid_feedback",
			"kind must be one of answer, citation, similar, search, face; "+
				"verdict must be one of helpful, not_helpful, wrong; a note is at most 500 characters")
	case errors.Is(err, files.ErrFeedbackTargetRequired):
		writeError(w, r, http.StatusBadRequest, "invalid_feedback",
			"this kind of feedback needs the thing it is about: a node_id, a person_id, or the question that was asked")
	default:
		s.writeFilesError(w, r, op, err)
	}
}

type feedbackRequest struct {
	Kind     string `json:"kind"`
	NodeID   string `json:"node_id"`
	PersonID string `json:"person_id"`
	Context  string `json:"context"`
	Verdict  string `json:"verdict"`
	Note     string `json:"note"`
}

// handleSubmitFeedback records or replaces one judgement.
//
// One endpoint for every kind rather than a route per result shape. The
// alternative — /chat/feedback, /nodes/{id}/similar/feedback and so on — would
// spread the access rule across five handlers, and the access rule is the part
// that must not vary.
func (s *Server) handleSubmitFeedback(w http.ResponseWriter, r *http.Request) {
	var body feedbackRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	in := files.Feedback{
		OwnerID: CurrentUser(r.Context()).ID,
		Kind:    strings.TrimSpace(body.Kind),
		Context: body.Context,
		Verdict: strings.TrimSpace(body.Verdict),
		Note:    body.Note,
	}

	if id := strings.TrimSpace(body.NodeID); id != "" {
		nodeID, err := uuid.Parse(id)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid node id")
			return
		}
		// The whole access rule, in one call, before anything is written. 404 for
		// "not yours" as well as "not there" — s.writeFilesError maps
		// files.ErrNotFound to exactly the message a missing file gets.
		if _, err := s.files.Store().AccessFor(r.Context(), in.OwnerID, nodeID); err != nil {
			s.writeFilesError(w, r, "feedback target", err)
			return
		}
		in.NodeID = &nodeID
	}

	if id := strings.TrimSpace(body.PersonID); id != "" {
		personID, err := uuid.Parse(id)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid person id")
			return
		}
		// People are already per-owner, so GetPerson answering ErrPersonNotFound
		// covers "somebody else's cluster" and "no such cluster" with one code —
		// the same collapse, arrived at from the other direction.
		if _, err := s.files.Store().GetPerson(r.Context(), in.OwnerID, personID); err != nil {
			s.writePeopleError(w, r, "feedback target", err)
			return
		}
		in.PersonID = &personID
	}

	out, err := s.files.Store().RecordFeedback(r.Context(), in)
	if err != nil {
		s.writeFeedbackError(w, r, "record feedback", err)
		return
	}

	// 201 for a first verdict and for a changed one alike. The resource is "this
	// person's standing judgement on this result" and it exists either way; a
	// client that had to tell an insert from an update in order to render a
	// highlighted button would be learning something it has no use for.
	writeJSON(w, r, http.StatusCreated, map[string]any{"feedback": feedbackJSON(out)})
}

// handleListFeedback reads back the caller's OWN feedback.
//
// Only ever their own. There is no query parameter that widens it and no admin
// variant, because "what did my users call wrong" is a different feature with a
// different consent story — see files.ListFeedback, where the absence of that
// parameter is the enforcement rather than a convention.
func (s *Server) handleListFeedback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := s.files.Store().ListFeedback(r.Context(), CurrentUser(r.Context()).ID,
		strings.TrimSpace(q.Get("kind")),
		atoiDefault(q.Get("limit"), 0), atoiDefault(q.Get("offset"), 0))
	if err != nil {
		s.writeFeedbackError(w, r, "list feedback", err)
		return
	}

	items := make([]map[string]any, 0, len(out))
	for _, f := range out {
		items = append(items, feedbackJSON(f))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"feedback": items,
		"count":    len(items),
	})
}
