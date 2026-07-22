package httpapi

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// maxUploadBytes caps a single non-resumable upload.
//
// 5 GiB is generous for the simple path; anything larger belongs on the
// resumable endpoint in slice 4, where an interrupted transfer does not mean
// starting over. The limit exists at all because an unbounded PUT is how a
// single client fills the pool with no way to stop it midway.
const maxUploadBytes = 5 << 30

// --- shared helpers ---------------------------------------------------------

// nodeJSON is the wire shape. blob keys and internal version ids stay out of
// it: they are storage detail, and exposing them would freeze the on-disk
// layout that Phase 2 is going to replace.
func nodeJSON(n *files.Node) map[string]any {
	out := map[string]any{
		"id":         n.ID,
		"kind":       n.Kind,
		"name":       n.Name,
		"path":       n.Path,
		"created_at": n.CreatedAt,
		"updated_at": n.UpdatedAt,
	}
	if n.ParentID != nil {
		out["parent_id"] = *n.ParentID
	}
	if n.IsFile() {
		out["size"] = n.Size
		out["mime"] = n.MIME
		// The key names the algorithm so the value stays verifiable: SHA-256
		// for whole-file blobs, BLAKE3-256 for chunked (manifest-backed) files.
		// A client that wants to check a download needs to know which to run.
		if len(n.ContentHash) > 0 {
			if n.ManifestID != nil {
				out["blake3"] = hex.EncodeToString(n.ContentHash)
			} else {
				out["sha256"] = hex.EncodeToString(n.ContentHash)
			}
		}
	}
	if n.TrashedAt != nil {
		out["trashed_at"] = *n.TrashedAt
	}
	return out
}

func nodesJSON(ns []*files.Node) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		out = append(out, nodeJSON(n))
	}
	return out
}

// writeFilesError maps domain errors onto status codes in one place, so every
// handler reports the same failure the same way.
func (s *Server) writeFilesError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, files.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "no such file or folder")
	case errors.Is(err, files.ErrNameTaken):
		writeError(w, r, http.StatusConflict, "name_taken", "something with that name is already here")
	case errors.Is(err, files.ErrInvalidName):
		writeError(w, r, http.StatusBadRequest, "invalid_name",
			"names cannot be empty, contain / \\ : * ? \" < > | or control characters, or end in a space or dot")
	case errors.Is(err, files.ErrNotAFolder):
		writeError(w, r, http.StatusBadRequest, "not_a_folder", "that is not a folder")
	case errors.Is(err, files.ErrNotAFile):
		writeError(w, r, http.StatusBadRequest, "not_a_file", "that is not a file")
	case errors.Is(err, files.ErrCycle):
		writeError(w, r, http.StatusBadRequest, "invalid_move", "a folder cannot be moved inside itself")
	case errors.Is(err, files.ErrRootReserved):
		writeError(w, r, http.StatusBadRequest, "root_reserved", "the root folder cannot be renamed, moved or deleted")
	case errors.Is(err, files.ErrNotTrashed):
		writeError(w, r, http.StatusConflict, "not_trashed", err.Error())
	case errors.Is(err, files.ErrQuota):
		// 507 rather than 403: this is a storage condition, and clients that
		// understand WebDAV already treat it as "stop, you are full".
		writeError(w, r, http.StatusInsufficientStorage, "quota_exceeded", "storage quota exceeded")
	default:
		s.serverError(w, r, op, err)
	}
}

// nodeIDParam resolves the {id} path segment, accepting "root" so a client can
// start browsing without a prior lookup.
func (s *Server) nodeIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := r.PathValue("id")
	if raw == "root" {
		root, err := s.files.Store().EnsureRoot(r.Context(), CurrentUser(r.Context()).ID)
		if err != nil {
			s.serverError(w, r, "ensure root", err)
			return uuid.Nil, false
		}
		return root.ID, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid node id")
		return uuid.Nil, false
	}
	return id, true
}

// --- browse -----------------------------------------------------------------

func (s *Server) handleGetRoot(w http.ResponseWriter, r *http.Request) {
	root, err := s.files.Store().EnsureRoot(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "ensure root", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"node": nodeJSON(root)})
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	node, err := s.files.Store().Get(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.writeFilesError(w, r, "get node", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"node": nodeJSON(node)})
}

func (s *Server) handleListChildren(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	user := CurrentUser(r.Context())

	parent, err := s.files.Store().GetLive(r.Context(), user.ID, id)
	if err != nil {
		s.writeFilesError(w, r, "get folder", err)
		return
	}
	if !parent.IsFolder() {
		writeError(w, r, http.StatusBadRequest, "not_a_folder", "that is not a folder")
		return
	}

	children, err := s.files.Store().ListChildren(r.Context(), user.ID, id)
	if err != nil {
		s.serverError(w, r, "list children", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"parent":   nodeJSON(parent),
		"children": nodesJSON(children),
	})
}

// handleResolvePath is what WebDAV and the sync client need: address a node by
// its path rather than by an id they would otherwise have to walk the tree for.
func (s *Server) handleResolvePath(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" || !strings.HasPrefix(p, "/") {
		writeError(w, r, http.StatusBadRequest, "invalid_path", "path must be absolute and start with /")
		return
	}
	// Trailing slashes are how clients spell "this is a directory"; the stored
	// path never has one, so strip it rather than failing to match.
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}

	node, err := s.files.Store().GetByPath(r.Context(), CurrentUser(r.Context()).ID, p)
	if err != nil {
		s.writeFilesError(w, r, "resolve path", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"node": nodeJSON(node)})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.files.Store().Usage(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "usage", err)
		return
	}
	body := map[string]any{
		"live_bytes":  usage.LiveBytes,
		"trash_bytes": usage.TrashBytes,
		"total_bytes": usage.TotalBytes(),
		"file_count":  usage.FileCount,
	}
	if usage.QuotaBytes != nil {
		body["quota_bytes"] = *usage.QuotaBytes
		body["available_bytes"] = *usage.QuotaBytes - usage.TotalBytes()
	}
	writeJSON(w, r, http.StatusOK, body)
}

// --- mutate -----------------------------------------------------------------

type createFolderRequest struct {
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
}

func (s *Server) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	var req createFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "expected a JSON body")
		return
	}
	user := CurrentUser(r.Context())

	parentID, ok := s.resolveParent(w, r, req.ParentID)
	if !ok {
		return
	}

	node, err := s.files.Store().CreateFolder(r.Context(), user.ID, parentID, req.Name)
	if err != nil {
		s.writeFilesError(w, r, "create folder", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, map[string]any{"node": nodeJSON(node)})
}

// resolveParent turns an optional parent id into a concrete one, defaulting to
// the user's root.
func (s *Server) resolveParent(w http.ResponseWriter, r *http.Request, raw string) (uuid.UUID, bool) {
	user := CurrentUser(r.Context())
	if raw == "" || raw == "root" {
		root, err := s.files.Store().EnsureRoot(r.Context(), user.ID)
		if err != nil {
			s.serverError(w, r, "ensure root", err)
			return uuid.Nil, false
		}
		return root.ID, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "parent_id is not a valid id")
		return uuid.Nil, false
	}
	return id, true
}

type patchNodeRequest struct {
	Name     *string `json:"name"`
	ParentID *string `json:"parent_id"`
}

// handlePatchNode renames and/or moves. One endpoint rather than two because
// a drag-and-drop that both moves and renames must be atomic — two calls would
// leave a visible intermediate state, and a failure between them would leave
// the file somewhere the user never put it.
func (s *Server) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	var req patchNodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "expected a JSON body")
		return
	}
	if req.Name == nil && req.ParentID == nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "provide name, parent_id, or both")
		return
	}

	var newParent uuid.UUID
	if req.ParentID != nil {
		var ok bool
		if newParent, ok = s.resolveParent(w, r, *req.ParentID); !ok {
			return
		}
	}
	var newName string
	if req.Name != nil {
		newName = *req.Name
	}

	node, err := s.files.Store().MoveOrRename(r.Context(), CurrentUser(r.Context()).ID, id, newParent, newName)
	if err != nil {
		s.writeFilesError(w, r, "move or rename", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"node": nodeJSON(node)})
}

// handleTrashNode soft-deletes. DELETE means "put in the trash"; permanent
// removal is an explicit second step, because a client bug that deletes the
// wrong node should be recoverable.
func (s *Server) handleTrashNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	n, err := s.files.Store().Trash(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.writeFilesError(w, r, "trash node", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "trashed", "nodes_affected": n})
}

func (s *Server) handleListTrash(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.files.Store().ListTrash(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "list trash", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"items": nodesJSON(nodes)})
}

func (s *Server) handleRestoreNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	node, err := s.files.Store().Restore(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.writeFilesError(w, r, "restore node", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"node": nodeJSON(node)})
}

func (s *Server) handlePurgeNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	if err := s.files.Store().Purge(r.Context(), CurrentUser(r.Context()).ID, id); err != nil {
		s.writeFilesError(w, r, "purge node", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "purged"})
}

func (s *Server) handleEmptyTrash(w http.ResponseWriter, r *http.Request) {
	n, err := s.files.Store().EmptyTrash(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "empty trash", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "emptied", "items_purged": n})
}

// --- upload -----------------------------------------------------------------

// handleUpload accepts either a raw body or a multipart form.
//
// Raw body is the primary path: it streams straight to disk with no buffering
// and no framing overhead, which is what curl, the sync client and WebDAV all
// want. Multipart exists because a plain HTML <form> cannot produce anything
// else, and slice 5's UI should not need JavaScript to upload a file.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	user := CurrentUser(r.Context())

	parentID, ok := s.resolveParent(w, r, r.URL.Query().Get("parent_id"))
	if !ok {
		return
	}

	// MaxBytesReader, not a Content-Length check: a chunked upload has no
	// declared length, and trusting the header would let a client claim 1 byte
	// and send a terabyte.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)

	ctype := r.Header.Get("Content-Type")
	mediaType, params, _ := mime.ParseMediaType(ctype)

	var (
		name   string
		reader io.Reader
	)

	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(r.Body, params["boundary"])
		part, err := nextFilePart(mr)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_upload", "no file part in the multipart body")
			return
		}
		defer part.Close()

		name = part.FileName()
		if q := r.URL.Query().Get("name"); q != "" {
			name = q
		}
		// A browser may send a full client-side path; keep only the last
		// segment, and never let it contain a separator.
		name = lastPathSegment(name)
		reader = part
		// The part's own header is more specific than the request's.
		ctype = part.Header.Get("Content-Type")
	} else {
		name = lastPathSegment(r.URL.Query().Get("name"))
		if name == "" {
			writeError(w, r, http.StatusBadRequest, "invalid_upload", "a ?name= query parameter is required")
			return
		}
		reader = r.Body
	}

	node, err := s.files.Upload(r.Context(), user.ID, parentID, name, reader, ctype)
	if err != nil {
		// MaxBytesReader surfaces as a generic error from deep inside the copy;
		// translate it rather than reporting a 500 for a client-side mistake.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "too_large",
				fmt.Sprintf("uploads through this endpoint are limited to %d bytes", int64(maxUploadBytes)))
			return
		}
		s.writeFilesError(w, r, "upload", err)
		return
	}

	s.metrics.UploadBytes.Add(float64(node.Size))
	writeJSON(w, r, http.StatusCreated, map[string]any{"node": nodeJSON(node)})
}

// nextFilePart skips ordinary form fields to find the first file part.
func nextFilePart(mr *multipart.Reader) (*multipart.Part, error) {
	for {
		part, err := mr.NextPart()
		if err != nil {
			return nil, err
		}
		if part.FileName() != "" {
			return part, nil
		}
		part.Close()
	}
}

// lastPathSegment reduces "C:\Users\me\report.pdf" or "docs/report.pdf" to
// "report.pdf". Both separators are handled because the client's platform is
// unknown and a Windows browser really does send backslashes.
func lastPathSegment(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSpace(name)
}

// --- download ---------------------------------------------------------------

// handleDownload serves file content.
//
// http.ServeContent does the heavy lifting: Range requests, If-Range,
// If-None-Match, If-Modified-Since and 206 responses are all subtle enough that
// a hand-rolled version would be wrong in ways only video scrubbing reveals.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}

	node, content, err := s.files.Open(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.writeFilesError(w, r, "open file", err)
		return
	}
	defer content.Close()

	// A strong ETag: the content hash genuinely identifies the bytes, so a
	// client can cache indefinitely and revalidate cheaply. Versions are
	// immutable, so this never goes stale for the wrong reason. Which algorithm
	// produced it (SHA-256 or BLAKE3) is irrelevant here — an ETag is an opaque
	// identity, and both are unique per content.
	if len(node.ContentHash) > 0 {
		w.Header().Set("ETag", `"`+hex.EncodeToString(node.ContentHash)+`"`)
	}

	w.Header().Set("Content-Type", node.MIME)
	// private, not public: these are one user's files, and a shared proxy cache
	// must never hold them. Shared links in Phase 2 get their own policy.
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	// Without this, a stored HTML or SVG file executes in the origin of the app
	// itself — same-origin stored XSS with access to the session cookie. This
	// header is the whole defence, since the files are served from the API
	// origin rather than a separate sandbox domain.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Disposition", contentDisposition(node.Name, r.URL.Query().Get("download") != ""))
	w.Header().Set("Accept-Ranges", "bytes")

	// Count what was actually sent, not the file's size. A Range request serves
	// a slice, a 304 serves nothing, and a HEAD serves no body at all — adding
	// node.Size for any of those would inflate the counter to the point where
	// "bytes served" stops meaning anything.
	counter := &countingWriter{ResponseWriter: w}
	http.ServeContent(counter, r, node.Name, node.UpdatedAt, content)
	s.metrics.DownloadBytes.Add(float64(counter.n))
}

// countingWriter records how many body bytes reached the client.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

// Unwrap keeps http.ResponseController working through this wrapper, which
// ServeContent needs for read deadlines on long transfers.
func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// contentDisposition builds a header safe for non-ASCII names.
//
// The bare filename= parameter cannot carry anything outside ASCII, so RFC 6266
// filename*= is used alongside it: old clients read the ASCII fallback, modern
// ones read the UTF-8 form. Quotes and backslashes are stripped from the
// fallback because an unescaped quote there ends the parameter early and lets a
// crafted filename inject header parameters.
func contentDisposition(name string, forceDownload bool) string {
	disp := "inline"
	if forceDownload {
		disp = "attachment"
	}

	ascii := make([]rune, 0, len(name))
	for _, r := range name {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			ascii = append(ascii, '_')
			continue
		}
		ascii = append(ascii, r)
	}
	fallback := strings.TrimSpace(string(ascii))
	if fallback == "" {
		fallback = "download"
	}

	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`,
		disp, fallback, url.PathEscape(name))
}

// --- search -----------------------------------------------------------------

// handleSearch finds nodes by filename fragment.
//
// Trigram matching over names and paths, not full-text search over content:
// filenames are not prose, and people search them by fragment. Content search
// arrives in Phase 4 with OCR and embeddings, where it belongs.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	query := files.SearchQuery{
		Text:           q.Get("q"),
		Kind:           q.Get("kind"),
		Under:          q.Get("under"),
		IncludeTrashed: q.Get("include_trashed") == "true",
		Limit:          atoiDefault(q.Get("limit"), 50),
		Offset:         atoiDefault(q.Get("offset"), 0),
	}

	results, err := s.files.Store().Search(r.Context(), CurrentUser(r.Context()).ID, query)
	if err != nil {
		if errors.Is(err, files.ErrInvalidName) {
			writeError(w, r, http.StatusBadRequest, "query_too_short", err.Error())
			return
		}
		s.serverError(w, r, "search", err)
		return
	}

	out := make([]map[string]any, 0, len(results))
	for _, res := range results {
		item := nodeJSON(res.Node)
		// Why a result matched, so "the query is not in this filename" has a
		// visible answer rather than looking like a bug.
		item["matched_path"] = res.MatchedPath
		out = append(out, item)
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"query":   query.Text,
		"results": out,
		"count":   len(out),
		// Whether another page exists. A total count would need a second
		// aggregate over the same trigram scan to answer a question no client
		// asks — "are there more" is what paging actually needs.
		"has_more": len(out) == query.Limit,
	})
}

func atoiDefault(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// --- admin ------------------------------------------------------------------

func (s *Server) handleFsck(w http.ResponseWriter, r *http.Request) {
	repair := r.URL.Query().Get("repair") == "true"

	report, err := s.files.Fsck(r.Context(), repair)
	if err != nil {
		s.serverError(w, r, "fsck", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"checked":            report.Checked,
		"chunks_checked":     report.ChunksChecked,
		"orphans":            len(report.Orphans),
		"orphan_bytes":       report.OrphanBytes,
		"missing":            report.Missing,
		"missing_chunks":     report.MissingChunks,
		"refcount_drift":     report.RefcountDrift,
		"size_mismatch":      report.SizeMismatch,
		"temp_files_removed": report.TempFilesRemoved,
		"repaired":           repair,
	})
}
