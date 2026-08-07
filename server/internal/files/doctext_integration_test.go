package files_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// The whole slice-2 payoff in one test: a file's text is extracted and the file
// becomes findable by a word that appears only inside it, never in its name —
// through the same search that matches filenames, flagged so the client can say
// why it matched.
func TestExtractionMakesContentSearchable(t *testing.T) {
	f := newFixture(t)

	// A token that appears only in the body, not the filename.
	token := "zqx" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	body := "Meeting notes for Q3. The secret keyword is " + token + ", noted here once."

	node, err := f.svc.Upload(f.ctx, f.user, f.root, "notes.txt", strings.NewReader(body), "text/plain")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	// Before extraction, the token is nowhere the search can see it.
	if res, err := f.store.Search(f.ctx, f.user, files.SearchQuery{Text: token}); err != nil {
		t.Fatal(err)
	} else if len(res) != 0 {
		t.Fatalf("token found before extraction: %d results", len(res))
	}

	// Run the extract handler exactly as the worker would.
	h := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	if err := h.Handle(f.ctx, &node.ID, f.user); err != nil {
		t.Fatalf("extract handle: %v", err)
	}

	// The text is cached against the file's content hash.
	if text, ok, err := f.store.DocText(f.ctx, node.ContentHash); err != nil || !ok {
		t.Fatalf("doc text not stored: ok=%v err=%v", ok, err)
	} else if !strings.Contains(text, token) {
		t.Errorf("stored text missing the token")
	}

	// Now the file is findable by the body word, flagged as a content match.
	res, err := f.store.Search(f.ctx, f.user, files.SearchQuery{Text: token})
	if err != nil {
		t.Fatal(err)
	}
	var found *files.SearchResult
	for _, r := range res {
		if r.Node.ID == node.ID {
			found = r
			break
		}
	}
	if found == nil {
		t.Fatalf("file not found by its content (%d results)", len(res))
	}
	if !found.MatchedContent {
		t.Error("result not flagged as a content match")
	}
}

// A second identical upload does not re-extract: the text is cached by content,
// so the handler short-circuits on the shared hash.
func TestExtractionDedupsByContent(t *testing.T) {
	f := newFixture(t)
	body := "shared content " + uuid.NewString()

	a, err := f.svc.Upload(f.ctx, f.user, f.root, "a.txt", strings.NewReader(body), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.svc.Upload(f.ctx, f.user, f.root, "b.txt", strings.NewReader(body), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if string(a.ContentHash) != string(b.ContentHash) {
		t.Fatal("identical uploads should share a content hash")
	}

	h := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	if err := h.Handle(f.ctx, &a.ID, f.user); err != nil {
		t.Fatal(err)
	}
	// The second file's content is already extracted; the handler completes without
	// storing again (verified by it simply not erroring and the row already there).
	if err := h.Handle(f.ctx, &b.ID, f.user); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.store.DocText(f.ctx, b.ContentHash); !ok {
		t.Error("shared content text missing")
	}
}
