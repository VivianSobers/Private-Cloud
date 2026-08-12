package media

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/google/uuid"
)

// Kind is the job kind this handler drains. Separate from extraction and
// embedding so the three can be split across machines independently: media work
// needs file bytes and CPU but no model, extraction needs bytes and tesseract,
// embedding needs a sidecar and no bytes at all.
const Kind = "media"

// ErrContentGone means the file was trashed or purged between the job being
// enqueued and the worker reaching it. Not a failure to retry.
var ErrContentGone = errors.New("content no longer available")

// FileContent is what the handler needs to analyse a node.
type FileContent struct {
	MIME        string
	ContentHash []byte
	Reader      io.ReadCloser
}

// Opener fetches a node's content, exactly as the extractor's does.
type Opener interface {
	OpenForMedia(ctx context.Context, ownerID, nodeID uuid.UUID) (FileContent, error)
}

// VariantStore persists what was rendered. Returning the displaced storage key
// lets the handler delete superseded bytes; see files.PutMediaVariant.
type VariantStore interface {
	// State reports what already exists for this content: whether it has been
	// analysed, the dimensions found, and which variants are present. All three
	// are needed to decide whether there is work left, which is why this replaced
	// a bare "has it been analysed".
	State(ctx context.Context, contentHash []byte) (hasMeta bool, width, height int, variants []string, err error)
	PutMediaMeta(ctx context.Context, contentHash []byte, m Meta) error
	PutMediaVariant(ctx context.Context, contentHash []byte, v StoredVariant) (replacedKey string, err error)
}

// BlobWriter stores rendered bytes and removes superseded ones.
type BlobWriter interface {
	PutBytes(ctx context.Context, data []byte) (key string, err error)
	Remove(ctx context.Context, key string) error
}

// Meta and StoredVariant mirror the store's types. Declared here so this package
// does not import files — files imports media, and the dependency must not run
// both ways. The worker's adapter converts between them, which is a few lines in
// one place rather than a cycle.
type Meta = Metadata

// StoredVariant is a rendered variant plus where its bytes landed.
type StoredVariant struct {
	Variant       string
	StorageKey    string
	MIME          string
	Size          int64
	Width, Height int
}

// Handler analyses one file's media metadata and renders its variants.
type Handler struct {
	opener Opener
	store  VariantStore
	blobs  BlobWriter
	log    *slog.Logger
}

func NewHandler(opener Opener, store VariantStore, blobs BlobWriter, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Handler{opener: opener, store: store, blobs: blobs, log: log}
}

// Handle analyses one node. Like the extract handler it returns nil for every
// "nothing to do" outcome — not media, content gone, already analysed — and a
// real error only for a transient failure worth retrying. Idempotent by content
// hash, so two files with identical bytes do the work once.
func (h *Handler) Handle(ctx context.Context, nodeID *uuid.UUID, ownerID uuid.UUID) error {
	if nodeID == nil {
		return nil
	}

	fc, err := h.opener.OpenForMedia(ctx, ownerID, *nodeID)
	if errors.Is(err, ErrContentGone) {
		return nil
	}
	if err != nil {
		return err
	}
	defer fc.Reader.Close()

	if len(fc.ContentHash) == 0 || !IsMedia(fc.MIME) {
		return nil
	}

	// Content-addressed dedup: identical bytes are analysed and rendered once.
	// Checked before reading the file, so a re-upload of a photo already in the
	// library costs one query rather than a decode.
	//
	// "Already analysed" is NOT the same as "already done", and treating it as
	// such is what made a missing thumbnail permanent. Variants are rendered
	// after the metadata row and are best effort, so a render that failed — a
	// full disk, a killed worker, a transient blob write — leaves exactly this
	// state: metadata present, thumbnails absent. Returning here meant the retry
	// hit the same early return, and so did `cloudctl jobs reindex --kind=media`,
	// which is the remedy fsck's MissingVariants field tells an operator to run.
	hasMeta, width, height, have, err := h.store.State(ctx, fc.ContentHash)
	if err != nil {
		return err
	}
	missing := missingVariants(ExpectedVariants(fc.MIME, width, height), have)
	if hasMeta && len(missing) == 0 {
		return nil
	}

	data, err := io.ReadAll(io.LimitReader(fc.Reader, MaxInputBytes))
	if err != nil {
		return err // an IO failure IS worth retrying
	}

	meta := Meta{Width: width, Height: height}
	if !hasMeta {
		meta, err = Analyze(fc.MIME, data)
		switch {
		case errors.Is(err, ErrUnsupported), errors.Is(err, ErrDecode):
			// A file that claims to be an image and is not, or one too large to
			// decode safely, is a completed job. It will not get better on a retry,
			// and burning five attempts on it delays real work.
			h.log.Debug("nothing to analyse", "node", *nodeID, "mime", fc.MIME, "reason", err)
			return nil
		case err != nil:
			return err
		}

		if err := h.store.PutMediaMeta(ctx, fc.ContentHash, meta); err != nil {
			return err
		}
		// Dimensions were unknown before the analysis, so what is expected could
		// not be computed until now.
		missing = missingVariants(ExpectedVariants(fc.MIME, meta.Width, meta.Height), have)
	}

	// Variants are best effort AFTER the metadata is stored, deliberately in
	// that order: dimensions and capture time are what a timeline needs, and a
	// renderer failure must not cost the file its place in one. A missing
	// thumbnail degrades a tile; missing metadata hides the photo entirely.
	if len(missing) > 0 {
		if err := h.renderVariants(ctx, fc, data, missing); err != nil {
			h.log.Warn("variant rendering failed", "node", *nodeID, "error", err)
		}
	}

	h.log.Info("analysed media", "node", *nodeID, "mime", fc.MIME,
		"dimensions", meta.Width, "x", meta.Height, "source", meta.Source)
	return nil
}

// missingVariants is the set difference: what should exist, minus what does.
func missingVariants(want, have []string) []string {
	present := make(map[string]bool, len(have))
	for _, v := range have {
		present[v] = true
	}
	var out []string
	for _, v := range want {
		if !present[v] {
			out = append(out, v)
		}
	}
	return out
}

// renderVariants renders only the names asked for, so a re-run that is repairing
// one lost thumbnail does not re-encode the preview beside it.
func (h *Handler) renderVariants(ctx context.Context, fc FileContent, data []byte, names []string) error {
	variants, err := Render(fc.MIME, data, names...)
	if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrDecode) {
		return nil // video, or an image only the header of which decoded
	}
	if err != nil {
		return err
	}

	for _, v := range variants {
		key, err := h.blobs.PutBytes(ctx, v.Data)
		if err != nil {
			return err
		}
		replaced, err := h.store.PutMediaVariant(ctx, fc.ContentHash, StoredVariant{
			Variant:    v.Name,
			StorageKey: key,
			MIME:       v.MIME,
			Size:       int64(len(v.Data)),
			Width:      v.Width,
			Height:     v.Height,
		})
		if err != nil {
			// The bytes are written but unreferenced. Remove them rather than
			// leaving an orphan for fsck: we know the key right here, and fsck
			// is a manual, occasional pass.
			_ = h.blobs.Remove(ctx, key)
			return err
		}
		if replaced != "" {
			// A previous rendering of the same variant, superseded. Its row no
			// longer points at these bytes, so nothing can reach them.
			if err := h.blobs.Remove(ctx, replaced); err != nil {
				h.log.Warn("could not remove a superseded variant", "key", replaced, "error", err)
			}
		}
	}
	return nil
}
