package extract

import (
	"context"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type fakeOpener struct {
	content FileContent
	err     error
	opened  int
}

func (f *fakeOpener) OpenForExtract(context.Context, uuid.UUID, uuid.UUID) (FileContent, error) {
	f.opened++
	if f.err != nil {
		return FileContent{}, f.err
	}
	// Fresh reader each call so a retry can read again.
	c := f.content
	c.Reader = io.NopCloser(strings.NewReader(readerText))
	return c, nil
}

// readerText is the payload the fake opener serves; kept package-level so the
// fake can hand out a fresh reader per call.
var readerText = "Receipt total 42.00 thank you"

type fakeStore struct {
	texts map[string]string
	has   map[string]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{texts: map[string]string{}, has: map[string]bool{}}
}
func (f *fakeStore) HasDocText(_ context.Context, h []byte) (bool, error) {
	return f.has[hex.EncodeToString(h)], nil
}
func (f *fakeStore) PutDocText(_ context.Context, h []byte, text, _, _ string) error {
	f.texts[hex.EncodeToString(h)] = text
	f.has[hex.EncodeToString(h)] = true
	return nil
}

func mustHash() []byte { return []byte{0xaa, 0xbb, 0xcc, 0xdd} }

func TestHandlerStoresExtractedText(t *testing.T) {
	opener := &fakeOpener{content: FileContent{MIME: "text/plain", ContentHash: mustHash()}}
	store := newFakeStore()
	h := NewHandler(opener, store, New(), nil)

	node := uuid.New()
	if err := h.Handle(context.Background(), &node, uuid.New()); err != nil {
		t.Fatalf("handle: %v", err)
	}
	got := store.texts[hex.EncodeToString(mustHash())]
	if !strings.Contains(got, "Receipt total 42.00") {
		t.Errorf("stored text wrong: %q", got)
	}
}

func TestHandlerIdempotentByContent(t *testing.T) {
	opener := &fakeOpener{content: FileContent{MIME: "text/plain", ContentHash: mustHash()}}
	store := newFakeStore()
	store.has[hex.EncodeToString(mustHash())] = true // already extracted
	h := NewHandler(opener, store, New(), nil)

	node := uuid.New()
	if err := h.Handle(context.Background(), &node, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if len(store.texts) != 0 {
		t.Error("re-extracted content that was already cached")
	}
}

func TestHandlerContentGoneIsNotAFailure(t *testing.T) {
	opener := &fakeOpener{err: ErrContentGone}
	store := newFakeStore()
	h := NewHandler(opener, store, New(), nil)

	node := uuid.New()
	if err := h.Handle(context.Background(), &node, uuid.New()); err != nil {
		t.Errorf("content-gone should complete the job, got %v", err)
	}
	if len(store.texts) != 0 {
		t.Error("stored text for gone content")
	}
}

func TestHandlerUnsupportedTypeCompletes(t *testing.T) {
	opener := &fakeOpener{content: FileContent{MIME: "application/octet-stream", ContentHash: mustHash()}}
	store := newFakeStore()
	h := NewHandler(opener, store, New(), nil)

	node := uuid.New()
	if err := h.Handle(context.Background(), &node, uuid.New()); err != nil {
		t.Errorf("unsupported type should complete the job, got %v", err)
	}
	if len(store.texts) != 0 {
		t.Error("stored text for an unsupported type")
	}
}
