package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
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
		// Server-sent events instead of one JSON object. See streamChat.
		Stream bool `json:"stream"`
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
	for _, c := range chunks {
		// A passage with no text is not a citation. fillChunkText leaves Text
		// empty when doc_text has been pruned or the chunker has changed shape
		// since the vector was written, and both are ordinary rather than
		// exceptional. Offering it to the generator is worse than dropping it: an
		// empty passage contributes nothing to the answer and everything to the
		// impression that the answer is grounded, since the citation beside it
		// names a real file the user can open and read something unrelated in.
		//
		// Dropped before the ref is assigned, so the numbering a reader sees has
		// no gaps in it.
		if strings.TrimSpace(c.Text) == "" {
			continue
		}
		ref := strconv.Itoa(len(citations) + 1)
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

	// Nothing citable. Either retrieval matched nothing, or everything it matched
	// has lost its text — and the answer is the same either way, because a model
	// asked to answer from no passages invents one, which is the exact failure
	// this design refuses to ship.
	if len(passages) == 0 {
		out["answer_unavailable"] = "no_matching_documents"
		if body.Stream {
			// Still a stream, because the client asked for one: a caller that set
			// up an event reader must not have to also handle a plain JSON body on
			// the days retrieval finds nothing.
			s.streamChat(w, r, out, nil, "")
			return
		}
		writeJSON(w, r, http.StatusOK, out)
		return
	}

	if s.generator == nil {
		// Retrieval-only mode. The passages are the useful, trustworthy half.
		out["answer_unavailable"] = "generation_disabled"
		if body.Stream {
			s.streamChat(w, r, out, nil, "")
			return
		}
		writeJSON(w, r, http.StatusOK, out)
		return
	}

	if body.Stream {
		s.streamChat(w, r, out, passages, question)
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

// streamChat delivers the same answer as Server-Sent Events.
//
// This was deferred through two phases for one reason, and the reason is worth
// stating because it also dictates the shape: citations are mandatory, and an
// answer that streamed ahead of its citations would be — for the whole duration
// of the stream — exactly the unverifiable output this design refuses to
// produce. A reader would watch a confident paragraph assemble itself with
// nothing yet saying where any of it came from.
//
// What unblocks it is that the citations do not depend on the answer. They are
// computed from retrieval, before the generator is called at all. So they are
// sent FIRST, as a complete event, and no token of prose is written until the
// client already holds every source the answer is grounded in. Streaming then
// costs nothing in verifiability and buys the thing it is worth having for: a
// model on modest hardware takes tens of seconds to finish a paragraph, and
// watching it arrive is the difference between a feature that feels broken and
// one that feels slow.
//
// The event sequence, which is the contract with the client:
//
//	event: citations   the full citation list, always, exactly once, first
//	event: delta       a piece of the answer; zero or more
//	event: done        the terminator, carrying model or answer_unavailable
//
// `done` is always sent, including on failure, because a stream that simply
// stops is indistinguishable from a network that dropped — and a client that
// cannot tell those apart either hangs forever or reports a failure that did not
// happen.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, out map[string]any, passages []embed.Passage, question string) {
	// Written before the first byte, since headers cannot be set afterwards.
	// no-transform and X-Accel-Buffering exist for the proxy in front: Caddy will
	// happily buffer a response into one write, which turns a stream back into a
	// slow whole answer without anything appearing to be wrong.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	send := func(event string, payload any) error {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		// Flushing per event is the whole point; without it the writer buffers and
		// the client sees one delivery at the end.
		return rc.Flush()
	}

	// Citations first, always, before a single token of prose.
	if err := send("citations", out); err != nil {
		return
	}

	// Nothing to generate: retrieval-only mode, or nothing citable. The reason is
	// already in `out`, so the terminator carries it and the stream ends here.
	if len(passages) == 0 {
		_ = send("done", map[string]any{"answer_unavailable": out["answer_unavailable"]})
		return
	}

	streamer, ok := s.generator.(embed.StreamingGenerator)
	if !ok {
		// A generator that cannot stream is still a generator. Ask for the whole
		// answer and deliver it as one delta: the client's rendering is identical,
		// it simply arrives at once.
		answer, err := s.generator.Generate(r.Context(), question, passages)
		if err != nil {
			s.log.Warn("generation failed", "error", err, "request_id", RequestID(r.Context()))
			_ = send("done", map[string]any{"answer_unavailable": "generation_unavailable"})
			return
		}
		_ = send("delta", map[string]any{"text": answer})
		_ = send("done", map[string]any{"model": s.generator.Model()})
		return
	}

	var wrote bool
	err := streamer.GenerateStream(r.Context(), question, passages, func(delta string) error {
		wrote = true
		return send("delta", map[string]any{"text": delta})
	})
	if err != nil {
		s.log.Warn("generation stream failed", "error", err, "wrote_any", wrote,
			"request_id", RequestID(r.Context()))
		// A failure after prose has already been sent is reported as a truncation
		// rather than as an absent answer: the client is holding half a paragraph,
		// and telling it the answer is unavailable would be a lie about something
		// the user can see on screen.
		reason := "generation_unavailable"
		if wrote {
			reason = "generation_truncated"
		}
		_ = send("done", map[string]any{"answer_unavailable": reason})
		return
	}
	_ = send("done", map[string]any{"model": s.generator.Model()})
}

// The contract also sketches scope.node_ids and scope.tags. Neither is
// implemented, and neither is accepted — decodeJSON refuses unknown fields, so a
// request naming one is answered 400 with the field's name. That is what makes
// this comment true rather than aspirational: it said the same thing while the
// decoder underneath quietly accepted both, which is exactly the failure it
// describes. A scope field that parses and then does nothing is worse than an
// absent one, because a caller would believe their question was narrowed when it
// was not.
