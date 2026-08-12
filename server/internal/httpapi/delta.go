package httpapi

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
)

// The delta protocol endpoints — the sync client's block-level transfer surface.
// A client fetches a file's manifest, pulls the chunks it lacks, and — uploading
// — asks which chunks the server already has and sends only the rest, then binds
// them into a file with a manifest commit.

// maxHaveHashes bounds a single existence query so one request cannot ask about
// an unbounded set.
const maxHaveHashes = 10000

// parseHashHex decodes a 64-char hex chunk address.
func parseHashHex(s string) ([32]byte, bool) {
	var h [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return h, false
	}
	copy(h[:], b)
	return h, true
}

// handleFileManifest describes how to fetch a file: its ordered chunk list when
// it is content-addressed, or a whole-file marker when it is a small blob the
// client should just download in one piece.
func (s *Server) handleFileManifest(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	node, err := s.files.Store().GetLive(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.writeFilesError(w, r, "get file", err)
		return
	}
	if !node.IsFile() {
		writeError(w, r, http.StatusBadRequest, "not_a_file", "that is not a file")
		return
	}

	if node.ManifestID == nil {
		// A whole-file blob: no manifest to diff, fetch it via /content.
		out := map[string]any{"kind": "whole", "size": node.Size}
		if len(node.ContentHash) > 0 {
			out["sha256"] = hex.EncodeToString(node.ContentHash)
		}
		writeJSON(w, r, http.StatusOK, out)
		return
	}

	if s.files.CAS() == nil {
		s.serverError(w, r, "manifest", errors.New("file is content-addressed but CAS is not configured"))
		return
	}
	entries, err := s.files.CAS().Entries(r.Context(), *node.ManifestID)
	if err != nil {
		s.serverError(w, r, "manifest entries", err)
		return
	}
	chunks := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		chunks = append(chunks, map[string]any{
			"hash":   hex.EncodeToString(e.Hash[:]),
			"offset": e.Offset,
			"size":   e.Size,
		})
	}
	out := map[string]any{"kind": "chunked", "total_size": node.Size, "chunks": chunks}
	if len(node.ContentHash) > 0 {
		out["blake3"] = hex.EncodeToString(node.ContentHash)
	}
	writeJSON(w, r, http.StatusOK, out)
}

type haveRequest struct {
	Hashes []string `json:"hashes"`
}

// handleHaveChunks answers which of the given chunk hashes the caller must
// upload, so a client sends only what it has to.
//
// It answers from the caller's OWN chunks, not from the global chunk table.
//
// The global answer was an existence oracle: ten thousand hashes per request,
// no ownership check, and a truthful yes/no about whether any given content is
// stored anywhere on this server. handleGetChunk goes to some trouble to avoid
// exactly that — it gates every read on UserReferencesChunk and its comment says
// so — and this endpoint handed the same information over for free, ten lines
// away.
//
// Scoping it costs a transfer, not a byte of disk. PutChunk writes through
// PutKeyed, which is a no-op when the key already exists, so a chunk uploaded
// because we declined to admit we had it is stored exactly once and the
// refcount rises as usual. Cross-user STORAGE dedup is untouched; what is gone
// is cross-user BANDWIDTH dedup, which is the half that was doing the leaking.
// Within one account — a file copied, a file re-uploaded, two files sharing a
// prefix — nothing changes at all, and that is where the delta protocol's
// savings actually come from.
func (s *Server) handleHaveChunks(w http.ResponseWriter, r *http.Request) {
	var req haveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "expected a JSON body")
		return
	}
	if len(req.Hashes) > maxHaveHashes {
		writeError(w, r, http.StatusBadRequest, "too_many", "too many hashes in one request")
		return
	}

	hashes := make([][32]byte, 0, len(req.Hashes))
	for _, hs := range req.Hashes {
		h, ok := parseHashHex(hs)
		if !ok {
			writeError(w, r, http.StatusBadRequest, "invalid_hash", "not a valid chunk hash")
			return
		}
		hashes = append(hashes, h)
	}

	held, err := s.files.Store().UserReferencesChunks(r.Context(), CurrentUser(r.Context()).ID, hashes)
	if err != nil {
		s.serverError(w, r, "have chunks", err)
		return
	}
	missing := make([]string, 0)
	for _, h := range hashes {
		if !held[h] {
			missing = append(missing, hex.EncodeToString(h[:]))
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"missing": missing})
}

// handleGetChunk serves one chunk's stored bytes, scoped to a user who already
// references it. The client decompresses per the header and verifies the bytes
// against the address — the same self-check the server does on every read.
func (s *Server) handleGetChunk(w http.ResponseWriter, r *http.Request) {
	hash, ok := parseHashHex(r.PathValue("hash"))
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_hash", "not a valid chunk hash")
		return
	}

	// A chunk is global, but reading one requires already holding a file made of
	// it. A user who does not is answered 404 — indistinguishable from the chunk
	// not existing, so this is not an existence oracle for other users' content.
	referenced, err := s.files.Store().UserReferencesChunk(r.Context(), CurrentUser(r.Context()).ID, hash)
	if err != nil {
		s.serverError(w, r, "chunk auth", err)
		return
	}
	if !referenced {
		writeError(w, r, http.StatusNotFound, "not_found", "no such chunk")
		return
	}

	stored, compression, size, err := s.files.CAS().ReadChunkStored(r.Context(), hash)
	if errors.Is(err, cas.ErrChunkNotFound) {
		writeError(w, r, http.StatusNotFound, "not_found", "no such chunk")
		return
	}
	if err != nil {
		s.serverError(w, r, "read chunk", err)
		return
	}

	// A chunk is immutable — its address IS its content — so it caches forever.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+r.PathValue("hash")+`"`)
	w.Header().Set("X-Chunk-Compression", compression)
	w.Header().Set("X-Chunk-Plain-Size", strconv.Itoa(size))
	_, _ = w.Write(stored)
}

// handlePutChunk stores one client-supplied plaintext chunk, verified against its
// claimed address. The verification lives in cas.PutChunk; this maps the outcome.
func (s *Server) handlePutChunk(w http.ResponseWriter, r *http.Request) {
	hash, ok := parseHashHex(r.PathValue("hash"))
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_hash", "not a valid chunk hash")
		return
	}

	// A chunk cannot legitimately exceed the max chunk size; cap the read so a
	// client cannot stream an unbounded body at this endpoint.
	r.Body = http.MaxBytesReader(w, r.Body, cas.MaxChunkSize+1)
	plain, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, r, http.StatusRequestEntityTooLarge, "too_large", "chunk exceeds the maximum chunk size")
		return
	}

	stored, err := s.files.CAS().PutChunk(r.Context(), hash, plain)
	switch {
	case errors.Is(err, cas.ErrHashMismatch):
		writeError(w, r, http.StatusBadRequest, "hash_mismatch", "chunk content does not match its hash")
		return
	case errors.Is(err, cas.ErrChunkTooLarge):
		writeError(w, r, http.StatusRequestEntityTooLarge, "too_large", "chunk exceeds the maximum chunk size")
		return
	case err != nil:
		s.serverError(w, r, "put chunk", err)
		return
	}

	status := http.StatusOK
	if stored {
		status = http.StatusCreated
	}
	writeJSON(w, r, status, map[string]any{"stored": stored})
}

type commitManifestRequest struct {
	ContentHash string   `json:"content_hash"`
	Chunks      []string `json:"chunks"`
	MIME        string   `json:"mime"`
}

// handleCommitManifest binds already-uploaded chunks into a file — the delta
// upload's final step, where the bytes that already transferred become a file
// version without moving again.
func (s *Server) handleCommitManifest(w http.ResponseWriter, r *http.Request) {
	parentID, ok := s.resolveParent(w, r, r.URL.Query().Get("parent_id"))
	if !ok {
		return
	}
	name := lastPathSegment(r.URL.Query().Get("name"))
	if name == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "a ?name= query parameter is required")
		return
	}

	var req commitManifestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "expected a JSON body")
		return
	}
	contentHash, ok := parseHashHex(req.ContentHash)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_hash", "content_hash is not a valid hash")
		return
	}
	hashes := make([][32]byte, 0, len(req.Chunks))
	for _, hs := range req.Chunks {
		h, ok := parseHashHex(hs)
		if !ok {
			writeError(w, r, http.StatusBadRequest, "invalid_hash", "a chunk hash is invalid")
			return
		}
		hashes = append(hashes, h)
	}

	node, err := s.files.PutManifestFile(r.Context(), CurrentUser(r.Context()).ID, parentID, name, contentHash, hashes, req.MIME)
	switch {
	case errors.Is(err, cas.ErrContentHashMismatch):
		// The named chunks do not reassemble to the declared hash. For an honest
		// client this means its local chunking and the server's disagree, which is
		// worth saying plainly rather than hiding behind a generic 400.
		writeError(w, r, http.StatusBadRequest, "content_hash_mismatch",
			"the named chunks do not reassemble to the declared content_hash")
		return
	case errors.Is(err, cas.ErrMissingChunk):
		writeError(w, r, http.StatusBadRequest, "missing_chunk", "a referenced chunk has not been uploaded")
		return
	case errors.Is(err, cas.ErrEmptyManifest):
		writeError(w, r, http.StatusBadRequest, "empty_manifest", "a manifest must reference at least one chunk")
		return
	case err != nil:
		s.writeFilesError(w, r, "commit manifest", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, map[string]any{"node": nodeJSON(node)})
}
