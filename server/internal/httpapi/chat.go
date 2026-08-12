package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
)

// Phase 8: ask a question of your own documents.
//
// Two halves with very different reliability. RETRIEVAL runs on the embedding
// space Phase 4 shipped and works wherever semantic search works. GENERATION
// needs a model on a GPU box, and is optional.
//
// When only retrieval is available this endpoint still answers 200 with the
// passages and no `answer`. That is deliberate: surfacing the source documents is
// the trustworthy half of RAG and is useful on its own — it is exactly what the
// web client's "Ask your library" view already renders. Refusing the whole
// request because the generator is absent would take away a working feature to
// punish the absence of an optional one.

// maxChatPassages bounds how much context is offered to the generator.
//
// Not a tuning knob for answer quality so much as a memory bound: every passage
// is tokens the model has to hold, and a question that swept in fifty of them
// would make one request the size of a small fine-tune batch on hardware that
// has one GPU at best.
const maxChatPassages = 8

// chatMinQuestion is the shortest question worth embedding. Below this the
// query vector is noise and the retrieval is arbitrary.
const chatMinQuestion = 3

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if s.embedder == nil {
		// Without embeddings there is no retrieval, and without retrieval there is
		// nothing to ground an answer in. Same stable code the search path uses, so
		// a client needs no new failure branch.
		writeError(w, r, http.StatusServiceUnavailable, "semantic_unavailable",
			"asking questions needs the embedding sidecar")
		return
	}

	var body struct {
		Question string `json:"question"`
		Scope    struct {
			Under string `json:"under"`
		} `json:"scope"`
		IncludeShared bool `json:"include_shared"`
		Limit         int  `json:"limit"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	question := strings.TrimSpace(body.Question)
	if len([]rune(question)) < chatMinQuestion {
		writeError(w, r, http.StatusBadRequest, "question_too_short",
			"ask a question of at least 3 characters")
		return
	}

	limit := body.Limit
	if limit <= 0 || limit > maxChatPassages {
		limit = maxChatPassages
	}

	vecs, err := s.embedder.Embed(r.Context(), []string{question})
	if err != nil || len(vecs) == 0 {
		s.log.Warn("question embedding failed", "error", err,
			"request_id", RequestID(r.Context()))
		writeError(w, r, http.StatusServiceUnavailable, "semantic_unavailable",
			"the embedding sidecar is temporarily unavailable")
		return
	}

	user := CurrentUser(r.Context())
	// Retrieval reaches only what this caller could already open. That is what
	// stops chat becoming a way to read around a permission — under Phase 7 the
	// ACL filter applies to retrieval for the same reason it applies to search.
	chunks, err := s.files.Store().RetrieveChunks(r.Context(), user.ID, vecs[0],
		s.embedder.Model(), limit, body.IncludeShared, strings.TrimRight(body.Scope.Under, "/"))
	if err != nil {
		s.serverError(w, r, "retrieve", err)
		return
	}

	citations := make([]map[string]any, 0, len(chunks))
	passages := make([]embed.Passage, 0, len(chunks))
	for i, c := range chunks {
		ref := strconv.Itoa(i + 1)
		citations = append(citations, map[string]any{
			"ref":       ref,
			"node_id":   c.Node.ID,
			"path":      c.Node.Path,
			"name":      c.Node.Name,
			"chunk_seq": c.Seq,
			"score":     c.Score,
		})
		passages = append(passages, embed.Passage{Ref: ref, Path: c.Node.Path, Text: c.Text})
	}

	out := map[string]any{
		"question": question,
		// Citations are mandatory, not decorative: an answer over someone's own
		// documents that cannot say which document it came from is unverifiable,
		// and a confident wrong answer about your own files is worse than no
		// feature. They are present whether or not an answer was generated.
		"citations": citations,
	}

	if len(chunks) == 0 {
		// Nothing retrieved. Answering anyway would be the model inventing
		// something, which is the exact failure this design refuses to ship.
		out["answer_unavailable"] = "no_matching_documents"
		writeJSON(w, r, http.StatusOK, out)
		return
	}

	if s.generator == nil {
		// Retrieval-only mode. The passages are the useful, trustworthy half.
		out["answer_unavailable"] = "generation_disabled"
		writeJSON(w, r, http.StatusOK, out)
		return
	}

	answer, err := s.generator.Generate(r.Context(), question, passages)
	if errors.Is(err, embed.ErrGenerationUnavailable) {
		// The sidecar is down or misbehaving. Degrade to retrieval rather than
		// failing: the client already renders citations, and losing the written
		// answer is a smaller loss than losing the whole response.
		s.log.Warn("generation failed", "error", err, "request_id", RequestID(r.Context()))
		out["answer_unavailable"] = "generation_unavailable"
		writeJSON(w, r, http.StatusOK, out)
		return
	}
	if err != nil {
		s.serverError(w, r, "generate", err)
		return
	}

	out["answer"] = answer
	out["model"] = s.generator.Model()
	writeJSON(w, r, http.StatusOK, out)
}

// The contract also sketches scope.node_ids and scope.tags. Neither is
// implemented, and neither is accepted — decodeJSON refuses unknown fields, so a
// request naming one is answered 400 with the field's name. That is what makes
// this comment true rather than aspirational: it said the same thing while the
// decoder underneath quietly accepted both, which is exactly the failure it
// describes. A scope field that parses and then does nothing is worse than an
// absent one, because a caller would believe their question was narrowed when it
// was not.
