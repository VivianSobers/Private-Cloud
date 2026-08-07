package extract

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/google/uuid"
)

// Kind is the job kind this handler drains.
const Kind = "extract"

// ErrContentGone means the file was trashed or purged between the job being
// enqueued and the worker reaching it. Not a failure to retry — there is simply
// nothing left to extract.
var ErrContentGone = errors.New("content no longer available")

// FileContent is what the handler needs to extract from a node.
type FileContent struct {
	MIME        string
	ContentHash []byte
	Reader      io.ReadCloser
}

// Opener fetches a node's content. Two implementations exist: a co-located one
// that reads the blob store directly (worker on the always-on box), and a remote
// one that pulls over the authenticated download API (worker on a GPU box). The
// handler does not care which; it only ever sees bytes.
type Opener interface {
	OpenForExtract(ctx context.Context, ownerID, nodeID uuid.UUID) (FileContent, error)
}

// TextStore caches extracted text by content hash.
type TextStore interface {
	HasDocText(ctx context.Context, contentHash []byte) (bool, error)
	DocText(ctx context.Context, contentHash []byte) (string, bool, error)
	PutDocText(ctx context.Context, contentHash []byte, text, lang, source string) error
}

// TagStore records a node's automatic tags. Optional: nil means tagging is off.
type TagStore interface {
	SetAutoTags(ctx context.Context, nodeID uuid.UUID, tags []string) error
}

// Handler extracts a file's text and caches it. It is the worker's `extract`
// handler, decoupled from the jobs package: the worker adapts a claimed job's
// node/owner into this call.
type Handler struct {
	opener    Opener
	store     TextStore
	tags      TagStore
	extractor *Extractor
	log       *slog.Logger

	// next, if set, is called once a node has extractable text — to enqueue the
	// follow-on embed job. Injected by the worker wiring so this package stays free
	// of the jobs package, and nil when semantic search is not enabled, in which
	// case extraction simply stops at searchable text.
	next func(ctx context.Context, nodeID *uuid.UUID, ownerID uuid.UUID)
}

func NewHandler(opener Opener, store TextStore, ex *Extractor, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Handler{opener: opener, store: store, extractor: ex, log: log}
}

// Tagging wires automatic tagging. Called at startup; nil-safe, so a deployment
// that does not want tags simply never calls it.
func (h *Handler) Tagging(tags TagStore) { h.tags = tags }

// applyTags derives and stores a node's automatic tags, best effort — a tagging
// failure must not fail an extraction that already produced searchable text.
func (h *Handler) applyTags(ctx context.Context, nodeID uuid.UUID, mime, text string) {
	if h.tags == nil {
		return
	}
	if err := h.tags.SetAutoTags(ctx, nodeID, Tags(mime, text)); err != nil {
		h.log.Warn("auto-tagging failed", "node", nodeID, "error", err)
	}
}

// Chain wires a follow-on step run after a node gains extractable text — the
// embed job. Called at startup when semantic search is enabled.
func (h *Handler) Chain(next func(ctx context.Context, nodeID *uuid.UUID, ownerID uuid.UUID)) {
	h.next = next
}

func (h *Handler) scheduleNext(ctx context.Context, nodeID *uuid.UUID, ownerID uuid.UUID) {
	if h.next != nil {
		h.next(ctx, nodeID, ownerID)
	}
}

// Handle extracts text for one node and stores it. It returns nil — a completed
// job — for every "nothing to index" outcome (unsupported type, no text, no OCR,
// content gone, already cached), and a real error only for a transient failure
// worth retrying. Idempotent by content hash, so a job that runs twice, or two
// jobs for identical content, do the work once.
func (h *Handler) Handle(ctx context.Context, nodeID *uuid.UUID, ownerID uuid.UUID) error {
	if nodeID == nil {
		return nil // an owner-scoped job is not an extraction; nothing to do
	}

	fc, err := h.opener.OpenForExtract(ctx, ownerID, *nodeID)
	if errors.Is(err, ErrContentGone) {
		return nil
	}
	if err != nil {
		return err
	}
	defer fc.Reader.Close()

	if len(fc.ContentHash) == 0 {
		return nil // no addressable content (should not happen for a file)
	}

	// Content-addressed dedup: identical bytes are extracted once.
	has, err := h.store.HasDocText(ctx, fc.ContentHash)
	if err != nil {
		return err
	}
	if has {
		// Already extracted — but this node still needs its own tags (tags are
		// per-node), and may still need embedding (its content was extracted via a
		// different file, or before semantic search was on). Tag from the cached
		// text so keyword tags are applied even to an identical re-upload.
		text, _, _ := h.store.DocText(ctx, fc.ContentHash)
		h.applyTags(ctx, *nodeID, fc.MIME, text)
		h.scheduleNext(ctx, nodeID, ownerID)
		return nil
	}

	res, err := h.extractor.Extract(ctx, fc.MIME, fc.Reader)
	switch {
	case errors.Is(err, ErrUnsupported), errors.Is(err, ErrNoText), errors.Is(err, ErrNoOCR):
		// No text to index — but the file still gets its MIME-category tags. A
		// video is tagged "video" though nothing was extracted from it.
		h.log.Debug("nothing to extract", "node", *nodeID, "mime", fc.MIME, "reason", err)
		h.applyTags(ctx, *nodeID, fc.MIME, "")
		return nil
	case err != nil:
		return err
	}

	h.log.Info("extracted text", "node", *nodeID, "source", res.Source, "chars", len(res.Text))
	if err := h.store.PutDocText(ctx, fc.ContentHash, res.Text, res.Lang, res.Source); err != nil {
		return err
	}
	h.applyTags(ctx, *nodeID, fc.MIME, res.Text)
	// The file now has text; schedule embedding if the chain is wired.
	h.scheduleNext(ctx, nodeID, ownerID)
	return nil
}
