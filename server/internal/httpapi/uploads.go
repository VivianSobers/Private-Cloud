package httpapi

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Resumable uploads, tus 1.0.0 (core + creation + termination + expiration).
//
// tus rather than a bespoke chunk protocol because the clients already exist:
// uppy and tus-js-client in the browser, tus-py-client and tusd's Go client
// elsewhere. Inventing our own framing would mean writing every one of those
// clients too, and getting resumption subtly wrong in each.
//
// Spec: https://tus.io/protocols/resumable-upload

const (
	tusVersion = "1.0.0"
	// tusMaxSize caps a single resumable upload at 1 TiB. Not a real limit for
	// this deployment — it exists so a client cannot declare an absurd length
	// and have the server reserve quota against it.
	tusMaxSize = 1 << 40
)

// tusHeaders are sent on every tus response, including errors. A client that
// gets a bare 404 with no Tus-Resumable cannot tell "wrong URL" from "this
// server does not speak tus".
func tusHeaders(w http.ResponseWriter) {
	w.Header().Set("Tus-Resumable", tusVersion)
	// Browsers can only read these cross-origin if they are explicitly exposed;
	// without it, a JS client sees the headers as absent and cannot resume.
	w.Header().Set("Access-Control-Expose-Headers",
		"Tus-Resumable, Upload-Offset, Upload-Length, Upload-Expires, Location, Upload-Metadata")
}

// tusError sends an error with the tus headers attached.
func (s *Server) tusError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	tusHeaders(w)
	writeError(w, r, status, code, msg)
}

// checkTusVersion enforces the Tus-Resumable header on requests that carry one.
// A client speaking a different version must be told so rather than having its
// bytes accepted under semantics it does not share.
func (s *Server) checkTusVersion(w http.ResponseWriter, r *http.Request) bool {
	v := r.Header.Get("Tus-Resumable")
	if v == "" || v == tusVersion {
		return true
	}
	tusHeaders(w)
	w.Header().Set("Tus-Version", tusVersion)
	s.tusError(w, r, http.StatusPreconditionFailed, "unsupported_version",
		fmt.Sprintf("this server speaks tus %s", tusVersion))
	return false
}

// handleTusOptions advertises capabilities. Unauthenticated on purpose: it
// reveals nothing but protocol support, and tus clients send it before they
// have anywhere to attach credentials.
func (s *Server) handleTusOptions(w http.ResponseWriter, r *http.Request) {
	tusHeaders(w)
	w.Header().Set("Tus-Version", tusVersion)
	w.Header().Set("Tus-Extension", "creation,termination,expiration")
	w.Header().Set("Tus-Max-Size", strconv.Itoa(tusMaxSize))
	w.WriteHeader(http.StatusNoContent)
}

// handleTusCreate starts an upload and returns its URL in Location.
func (s *Server) handleTusCreate(w http.ResponseWriter, r *http.Request) {
	if !s.checkTusVersion(w, r) {
		return
	}
	if !s.files.SupportsResumable() {
		s.tusError(w, r, http.StatusNotImplemented, "not_supported",
			"the configured storage backend does not support resumable uploads")
		return
	}

	length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil || length < 0 {
		// Upload-Defer-Length is deliberately unsupported: without a declared
		// size there is no way to check quota before accepting the bytes, and
		// discovering at 99% that a file will not fit is the worst possible
		// moment to find out.
		s.tusError(w, r, http.StatusBadRequest, "invalid_length",
			"Upload-Length is required and must be a non-negative integer")
		return
	}
	if length > tusMaxSize {
		s.tusError(w, r, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("uploads are limited to %d bytes", int64(tusMaxSize)))
		return
	}

	meta := parseUploadMetadata(r.Header.Get("Upload-Metadata"))
	name := lastPathSegment(meta["filename"])
	if name == "" {
		name = lastPathSegment(r.URL.Query().Get("name"))
	}
	if name == "" {
		s.tusError(w, r, http.StatusBadRequest, "invalid_request",
			"a filename is required, in Upload-Metadata or as ?name=")
		return
	}

	parentID, ok := s.resolveParent(w, r, firstNonEmpty(meta["parent_id"], r.URL.Query().Get("parent_id")))
	if !ok {
		return
	}

	sess, err := s.files.CreateUpload(r.Context(), CurrentUser(r.Context()).ID,
		parentID, name, length, meta["filetype"])
	if err != nil {
		s.writeUploadError(w, r, "create upload", err)
		return
	}

	tusHeaders(w)
	w.Header().Set("Location", "/api/v1/uploads/"+sess.ID.String())
	w.Header().Set("Upload-Offset", "0")
	w.Header().Set("Upload-Length", strconv.FormatInt(sess.Size, 10))
	w.Header().Set("Upload-Expires", sess.ExpiresAt.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusCreated)
}

// handleTusHead reports how far an upload got. This is the resume path: a
// client that lost its connection asks here before deciding what to send.
func (s *Server) handleTusHead(w http.ResponseWriter, r *http.Request) {
	if !s.checkTusVersion(w, r) {
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}

	sess, err := s.files.Store().GetUpload(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.writeUploadError(w, r, "get upload", err)
		return
	}

	tusHeaders(w)
	w.Header().Set("Upload-Offset", strconv.FormatInt(sess.Offset, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(sess.Size, 10))
	w.Header().Set("Upload-Expires", sess.ExpiresAt.UTC().Format(http.TimeFormat))
	// A cached HEAD would tell a resuming client to restart from a stale
	// offset, which is the one answer that must never be wrong.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// handleTusPatch appends bytes at the client's stated offset.
func (s *Server) handleTusPatch(w http.ResponseWriter, r *http.Request) {
	if !s.checkTusVersion(w, r) {
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/offset+octet-stream" {
		s.tusError(w, r, http.StatusUnsupportedMediaType, "invalid_content_type",
			"Content-Type must be application/offset+octet-stream")
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		s.tusError(w, r, http.StatusBadRequest, "invalid_offset",
			"Upload-Offset is required and must be a non-negative integer")
		return
	}

	user := CurrentUser(r.Context())
	sess, err := s.files.AppendChunk(r.Context(), user.ID, id, offset, r.Body)
	if err != nil {
		s.writeUploadError(w, r, "append chunk", err)
		return
	}
	s.metrics.UploadBytes.Add(float64(sess.Offset - offset))

	tusHeaders(w)
	w.Header().Set("Upload-Offset", strconv.FormatInt(sess.Offset, 10))
	w.Header().Set("Upload-Expires", sess.ExpiresAt.UTC().Format(http.TimeFormat))

	if !sess.Complete() {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Finishing is automatic on the last byte. tus has no "commit" step, and
	// requiring an extra call would leave a completed upload sitting in staging
	// whenever a client disconnected right after its final chunk.
	node, err := s.files.FinishUpload(r.Context(), user.ID, id)
	if err != nil {
		s.writeUploadError(w, r, "finish upload", err)
		return
	}

	// The node id is returned in a custom header because tus reserves the body
	// of a PATCH response. Clients that ignore it can still find the file by
	// path; clients that read it avoid a lookup.
	w.Header().Set("X-Node-Id", node.ID.String())
	w.Header().Set("X-Node-Path", node.Path)
	w.WriteHeader(http.StatusNoContent)
}

// handleTusDelete cancels an upload and discards its bytes.
func (s *Server) handleTusDelete(w http.ResponseWriter, r *http.Request) {
	if !s.checkTusVersion(w, r) {
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	if err := s.files.CancelUpload(r.Context(), CurrentUser(r.Context()).ID, id); err != nil {
		s.writeUploadError(w, r, "cancel upload", err)
		return
	}
	tusHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ----------------------------------------------------------------

func (s *Server) writeUploadError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, files.ErrUploadNotFound):
		s.tusError(w, r, http.StatusNotFound, "not_found", "no such upload")
	case errors.Is(err, files.ErrOffsetMismatch):
		// 409 is what the spec mandates. Accepting a mismatched offset would
		// either duplicate or skip a range of the file.
		s.tusError(w, r, http.StatusConflict, "offset_mismatch", err.Error())
	case errors.Is(err, files.ErrUploadLocked):
		s.tusError(w, r, http.StatusLocked, "upload_locked",
			"another request is writing to this upload")
	case errors.Is(err, files.ErrUploadTooLarge):
		s.tusError(w, r, http.StatusRequestEntityTooLarge, "too_large",
			"more data was sent than Upload-Length declared")
	case errors.Is(err, files.ErrQuota):
		s.tusError(w, r, http.StatusInsufficientStorage, "quota_exceeded", "storage quota exceeded")
	case errors.Is(err, files.ErrNotFound):
		s.tusError(w, r, http.StatusNotFound, "not_found", "no such folder")
	case errors.Is(err, files.ErrNameTaken):
		s.tusError(w, r, http.StatusConflict, "name_taken", "something with that name is already here")
	case errors.Is(err, files.ErrInvalidName):
		s.tusError(w, r, http.StatusBadRequest, "invalid_name", "that filename is not allowed")
	case errors.Is(err, files.ErrNotAFolder):
		s.tusError(w, r, http.StatusBadRequest, "not_a_folder", "the upload target is not a folder")
	default:
		tusHeaders(w)
		s.serverError(w, r, op, err)
	}
}

func parseUUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid upload id")
		return uuid.Nil, false
	}
	return id, true
}

// parseUploadMetadata decodes the tus Upload-Metadata header:
//
//	filename <base64>,filetype <base64>,parent_id <base64>
//
// Base64 because the header must be ASCII and filenames are not. A pair with no
// value is legal per the spec and decodes to an empty string.
func parseUploadMetadata(header string) map[string]string {
	out := make(map[string]string)
	if header == "" {
		return out
	}
	for _, pair := range strings.Split(header, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, encoded, hasValue := strings.Cut(pair, " ")
		if !hasValue {
			out[key] = ""
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			// A malformed pair is skipped rather than failing the whole
			// request: one unparseable optional field should not stop an
			// upload whose filename decoded perfectly well.
			continue
		}
		out[key] = string(decoded)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
