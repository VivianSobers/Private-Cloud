package embed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/google/uuid"
)

// ImageKind is the job kind that fills the image-embedding space.
//
// Its own kind rather than extra work inside the media job, for FaceKind's
// reason: media analysis wants CPU and file bytes, image embedding wants a
// vision model on a GPU, and folding the second into the first would tie
// thumbnailing — which every deployment wants — to a sidecar most will not run.
const ImageKind = "image_embed"

// MaxImageBytes bounds what is read off a file and sent to the sidecar. Equal
// to media.MaxInputBytes, and the same number the reference sidecar refuses
// above: the two have to agree or the worker spends a tailnet round trip
// discovering a limit it already knew about.
const MaxImageBytes = 40 << 20

// ErrImageGone means the file was trashed or purged between the job being
// enqueued and the worker reaching it. Not a failure worth retrying.
//
// Declared here rather than reusing media.ErrContentGone because this package
// must not import media: files imports both, and the adapter that joins them
// lives there. It is one sentinel in one place, which is cheaper than the import
// cycle the alternative would create.
var ErrImageGone = errors.New("image content no longer available")

// ImageContent is what the handler needs to embed one node.
type ImageContent struct {
	MIME        string
	ContentHash []byte
	Reader      io.ReadCloser
}

// ImageOpener yields a node's bytes. Implemented in files, which already knows
// how to open content; this package stays pure of the database and the blob
// store so it can be tested on bytes alone.
type ImageOpener interface {
	OpenForImageEmbed(ctx context.Context, ownerID, nodeID uuid.UUID) (ImageContent, error)
}

// ImageVectorStore persists image embeddings, content-addressed and per model.
type ImageVectorStore interface {
	HasImageEmbedding(ctx context.Context, contentHash []byte, model string) (bool, error)
	PutImageEmbedding(ctx context.Context, contentHash []byte, model string, dim int, vector []float32) error
}

// ImageHandler embeds one image and stores its vector.
type ImageHandler struct {
	opener   ImageOpener
	store    ImageVectorStore
	embedder ImageEmbedder
	log      *slog.Logger
}

func NewImageHandler(opener ImageOpener, store ImageVectorStore, embedder ImageEmbedder, log *slog.Logger) *ImageHandler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &ImageHandler{opener: opener, store: store, embedder: embedder, log: log}
}

// Handle embeds one node's image content.
//
// Returns nil for every "nothing to do" outcome — not an image, content gone,
// already embedded — and a real error only for something a retry could fix. A
// video or a spreadsheet reaching this handler is a COMPLETED job, not a failed
// one: the backfill selects on MIME prefix and a library is mostly things this
// cannot embed, so treating them as failures would dead-letter most of a
// reindex.
//
// Idempotent by (content hash, model), which is what makes running the backfill
// twice free.
func (h *ImageHandler) Handle(ctx context.Context, nodeID *uuid.UUID, ownerID uuid.UUID) error {
	if nodeID == nil {
		return nil
	}

	ic, err := h.opener.OpenForImageEmbed(ctx, ownerID, *nodeID)
	if errors.Is(err, ErrImageGone) {
		return nil
	}
	if err != nil {
		return err
	}
	defer ic.Reader.Close()

	if !EmbeddableImage(ic.MIME) || len(ic.ContentHash) == 0 {
		return nil
	}

	// Checked BEFORE the bytes are read, not after: the point of content
	// addressing here is that the same picture uploaded twice costs one forward
	// pass, and reading 40 MiB off a spinning disk to then discard it would give
	// away most of that saving.
	model := h.embedder.Model()
	has, err := h.store.HasImageEmbedding(ctx, ic.ContentHash, model)
	if err != nil {
		return err
	}
	if has {
		return nil
	}

	data, err := io.ReadAll(io.LimitReader(ic.Reader, MaxImageBytes))
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}

	vector, err := h.embedder.EmbedImage(ctx, ic.MIME, data)
	if err != nil {
		// Every failure from the client is wrapped in ErrImageEmbedUnavailable,
		// including a width mismatch. Returned as an error so the queue's
		// backoff applies — the sidecar may simply be restarting, and a job
		// marked done with no vector is a photo that never gets one.
		return err
	}
	if len(vector) != h.embedder.Dim() {
		// Belt and braces: the client already refuses this, and the table's
		// CHECK would refuse it again. A vector of the wrong width is invisible
		// to the ranking filter forever, which looks exactly like a photo that
		// was never indexed.
		return nil
	}

	h.log.Info("embedded image", "node", *nodeID, "model", model, "dim", len(vector))
	return h.store.PutImageEmbedding(ctx, ic.ContentHash, model, h.embedder.Dim(), vector)
}

// EmbeddableImage reports whether a content type is one the image sidecar is
// expected to read.
//
// An allowlist rather than an `image/` prefix test, for media.isImage's reason:
// SVG is `image/*` and is not a raster image. It is deliberately WIDER than
// media's list, which is bounded by what the Go standard library decodes — this
// process never decodes anything, it forwards bytes to a service whose Pillow
// reads webp, tiff and bmp perfectly well. The sidecar is the authority on what
// it can actually read and answers "no faces"-style empty for the rest; this
// only decides what is worth a round trip.
func EmbeddableImage(contentType string) bool {
	if i := strings.Index(contentType, ";"); i >= 0 {
		contentType = contentType[:i]
	}
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/jpeg", "image/jpg", "image/png", "image/gif",
		"image/webp", "image/tiff", "image/bmp":
		return true
	}
	return false
}
