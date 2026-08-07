package embed

import (
	"context"
	"io"
	"log/slog"

	"github.com/google/uuid"
)

// Kind is the job kind this handler drains. Separate from extraction so the two
// can run on different workers: extraction on the always-on box, embedding on a
// GPU box, each claiming only its own kind.
const Kind = "embed"

// DocSource yields a node's content hash and already-extracted text. Embedding
// runs after extraction, so the text is expected to be present; if it is not
// (the extraction has not landed yet), the job is a no-op and re-runs later.
type DocSource interface {
	DocForNode(ctx context.Context, ownerID, nodeID uuid.UUID) (contentHash []byte, text string, ok bool, err error)
}

// VectorStore persists embeddings, content-addressed and per model.
type VectorStore interface {
	HasEmbedding(ctx context.Context, contentHash []byte, model string) (bool, error)
	PutEmbeddings(ctx context.Context, contentHash []byte, model string, dim int, vectors [][]float32) error
}

// Handler embeds a document's text and stores the vectors.
type Handler struct {
	docs     DocSource
	store    VectorStore
	embedder Embedder
	log      *slog.Logger
}

func NewHandler(docs DocSource, store VectorStore, embedder Embedder, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Handler{docs: docs, store: store, embedder: embedder, log: log}
}

// Handle embeds one node's text. Like extraction it returns nil for the "nothing
// to do" cases (no text yet, already embedded) and a real error only for a
// transient failure worth retrying — a sidecar timeout, say, which backoff then
// retries against a sidecar that may since have come back up. Idempotent by
// (content hash, model).
func (h *Handler) Handle(ctx context.Context, nodeID *uuid.UUID, ownerID uuid.UUID) error {
	if nodeID == nil {
		return nil
	}
	contentHash, text, ok, err := h.docs.DocForNode(ctx, ownerID, *nodeID)
	if err != nil {
		return err
	}
	if !ok || len(contentHash) == 0 {
		// No extracted text (yet), or the file is gone. Nothing to embed.
		return nil
	}

	model := h.embedder.Model()
	has, err := h.store.HasEmbedding(ctx, contentHash, model)
	if err != nil {
		return err
	}
	if has {
		return nil // this content is already embedded in this model's space
	}

	chunks := ChunkText(text)
	if len(chunks) == 0 {
		return nil
	}
	vectors, err := h.embedder.Embed(ctx, chunks)
	if err != nil {
		return err // transient: retry with backoff
	}

	h.log.Info("embedded document", "node", *nodeID, "chunks", len(chunks), "model", model)
	return h.store.PutEmbeddings(ctx, contentHash, model, h.embedder.Dim(), vectors)
}
