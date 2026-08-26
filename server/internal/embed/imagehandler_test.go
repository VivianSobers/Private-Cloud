package embed

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
)

// --- fakes -------------------------------------------------------------------

// fakeOpener hands the handler bytes without a database or a blob store, which
// is the point of the opener being an interface.
type fakeOpener struct {
	mime   string
	hash   []byte
	data   []byte
	err    error
	closed bool
	opens  int
}

func (o *fakeOpener) OpenForImageEmbed(context.Context, uuid.UUID, uuid.UUID) (ImageContent, error) {
	o.opens++
	if o.err != nil {
		return ImageContent{}, o.err
	}
	return ImageContent{
		MIME:        o.mime,
		ContentHash: o.hash,
		Reader:      closeTracker{Reader: bytes.NewReader(o.data), closed: &o.closed},
	}, nil
}

type closeTracker struct {
	io.Reader
	closed *bool
}

func (c closeTracker) Close() error { *c.closed = true; return nil }

// fakeVectorStore records what was written, keyed the way the table is.
type fakeVectorStore struct {
	have    map[string]bool
	written map[string][]float32
}

func newFakeVectorStore() *fakeVectorStore {
	return &fakeVectorStore{have: map[string]bool{}, written: map[string][]float32{}}
}

func (s *fakeVectorStore) HasImageEmbedding(_ context.Context, hash []byte, model string) (bool, error) {
	return s.have[string(hash)+"|"+model], nil
}

func (s *fakeVectorStore) PutImageEmbedding(_ context.Context, hash []byte, model string, _ int, v []float32) error {
	s.written[string(hash)+"|"+model] = v
	return nil
}

// fakeImageEmbedder is a deterministic stand-in for a vision model: the vector
// is derived from the bytes, so a test can prove the right content reached it.
type fakeImageEmbedder struct {
	dim   int
	err   error
	calls int
}

func (e *fakeImageEmbedder) Model() string { return "fake-clip" }
func (e *fakeImageEmbedder) Dim() int      { return e.dim }
func (e *fakeImageEmbedder) EmbedImage(_ context.Context, _ string, data []byte) ([]float32, error) {
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	v := make([]float32, e.dim)
	for i, b := range data {
		v[i%e.dim] += float32(b)
	}
	return v, nil
}

func handlerFor(o *fakeOpener, s *fakeVectorStore, e *fakeImageEmbedder) *ImageHandler {
	return NewImageHandler(o, s, e, nil)
}

// --- tests -------------------------------------------------------------------

func TestImageHandlerStoresOneVectorPerImage(t *testing.T) {
	o := &fakeOpener{mime: "image/jpeg", hash: []byte("hash-a"), data: []byte("jpegbytes")}
	store := newFakeVectorStore()
	emb := &fakeImageEmbedder{dim: 4}
	id := uuid.New()

	if err := handlerFor(o, store, emb).Handle(context.Background(), &id, uuid.New()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := store.written["hash-a|fake-clip"]; len(got) != 4 {
		t.Fatalf("stored vector = %v, want one 4-wide vector", got)
	}
	if !o.closed {
		t.Error("the content reader was not closed")
	}
}

// Idempotent by (content hash, model), and the check happens BEFORE the bytes
// are read — the whole point of content-addressing here is that the same picture
// uploaded twice costs one forward pass, and reading 40 MiB off a spinning disk
// to then discard it would give most of that back.
func TestImageHandlerSkipsContentAlreadyEmbedded(t *testing.T) {
	o := &fakeOpener{mime: "image/png", hash: []byte("hash-b"), data: []byte("pngbytes")}
	store := newFakeVectorStore()
	store.have["hash-b|fake-clip"] = true
	emb := &fakeImageEmbedder{dim: 4}
	id := uuid.New()

	if err := handlerFor(o, store, emb).Handle(context.Background(), &id, uuid.New()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if emb.calls != 0 {
		t.Errorf("the sidecar was called %d times for content already embedded", emb.calls)
	}
	if len(store.written) != 0 {
		t.Error("an already-embedded content hash was rewritten")
	}
}

// A file this space cannot embed is a COMPLETED job, not a failed one. The
// backfill selects on a MIME prefix that includes video, and a library is mostly
// things this cannot embed — treating them as failures would dead-letter most of
// a reindex.
func TestImageHandlerCompletesForNonImages(t *testing.T) {
	for _, mime := range []string{"video/mp4", "application/pdf", "image/svg+xml", "text/plain"} {
		o := &fakeOpener{mime: mime, hash: []byte("hash-c"), data: []byte("bytes")}
		store := newFakeVectorStore()
		emb := &fakeImageEmbedder{dim: 4}
		id := uuid.New()

		if err := handlerFor(o, store, emb).Handle(context.Background(), &id, uuid.New()); err != nil {
			t.Fatalf("Handle(%s) = %v, want nil", mime, err)
		}
		if emb.calls != 0 {
			t.Errorf("%s was sent to the sidecar", mime)
		}
	}
}

// Content that went away between enqueue and now is not a failure worth
// retrying: the file is gone and will stay gone.
func TestImageHandlerIgnoresVanishedContent(t *testing.T) {
	o := &fakeOpener{err: ErrImageGone}
	store := newFakeVectorStore()
	id := uuid.New()

	if err := handlerFor(o, store, &fakeImageEmbedder{dim: 4}).Handle(context.Background(), &id, uuid.New()); err != nil {
		t.Fatalf("Handle on vanished content = %v, want nil", err)
	}
}

// An unavailable sidecar goes BACK to the queue rather than being swallowed. It
// may simply be restarting, and a job marked done with no vector is a photo that
// never gets one.
func TestImageHandlerRetriesAnUnavailableSidecar(t *testing.T) {
	o := &fakeOpener{mime: "image/jpeg", hash: []byte("hash-d"), data: []byte("bytes")}
	store := newFakeVectorStore()
	emb := &fakeImageEmbedder{dim: 4, err: ErrImageEmbedUnavailable}
	id := uuid.New()

	err := handlerFor(o, store, emb).Handle(context.Background(), &id, uuid.New())
	if !errors.Is(err, ErrImageEmbedUnavailable) {
		t.Fatalf("Handle with a dead sidecar = %v, want ErrImageEmbedUnavailable", err)
	}
	if len(store.written) != 0 {
		t.Error("a vector was stored despite the sidecar failing")
	}
}

// A nil node id is a job with nothing to point at; it completes rather than
// panicking, exactly as every other handler does.
func TestImageHandlerToleratesANilNode(t *testing.T) {
	o := &fakeOpener{mime: "image/jpeg"}
	if err := handlerFor(o, newFakeVectorStore(), &fakeImageEmbedder{dim: 4}).
		Handle(context.Background(), nil, uuid.New()); err != nil {
		t.Fatalf("Handle(nil) = %v, want nil", err)
	}
	if o.opens != 0 {
		t.Error("a nil node still opened content")
	}
}

// The allowlist is wider than the media package's decoder allowlist and narrower
// than an `image/` prefix test, and both halves matter: this process never
// decodes, so webp is worth a round trip, and SVG is `image/*` and is not a
// raster image.
func TestEmbeddableImage(t *testing.T) {
	for _, mime := range []string{"image/jpeg", "image/png", "image/webp", "IMAGE/JPEG", "image/jpeg; charset=binary"} {
		if !EmbeddableImage(mime) {
			t.Errorf("EmbeddableImage(%q) = false, want true", mime)
		}
	}
	for _, mime := range []string{"image/svg+xml", "video/mp4", "application/pdf", "", "image"} {
		if EmbeddableImage(mime) {
			t.Errorf("EmbeddableImage(%q) = true, want false", mime)
		}
	}
}
