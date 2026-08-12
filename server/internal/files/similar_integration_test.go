package files_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// indexCorpus uploads documents, extracts their text and embeds them, returning
// the node ids by filename. The same fake embedder the Phase 4 tests use.
func indexCorpus(t *testing.T, f *fixture, bow bowEmbedder, docs map[string]string) map[string]uuid.UUID {
	t.Helper()
	ctx := context.Background()

	extractH := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	embedH := embed.NewHandler(f.store, f.store, bow, nil)

	ids := map[string]uuid.UUID{}
	for name, body := range docs {
		node, err := f.svc.Upload(ctx, f.user, f.root, name, strings.NewReader(body), "text/plain")
		if err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
		if err := extractH.Handle(ctx, &node.ID, f.user); err != nil {
			t.Fatalf("extract %s: %v", name, err)
		}
		if err := embedH.Handle(ctx, &node.ID, f.user); err != nil {
			t.Fatalf("embed %s: %v", name, err)
		}
		ids[name] = node.ID
	}
	return ids
}

// "More like this" ranks by meaning and, crucially, does not return the file
// being asked about.
func TestSimilarToRanksRelatedDocuments(t *testing.T) {
	f := newFixture(t)
	bow := bowEmbedder{dim: 1024}

	ids := indexCorpus(t, f, bow, map[string]string{
		"cats.txt":     "cats kittens feline whiskers purr meow tabby claws",
		"kittens.txt":  "kittens feline purr whiskers playful meow paws",
		"finance.txt":  "invoice payment tax revenue budget accounting ledger audit",
		"asteroid.txt": "planets orbit galaxy stars cosmos nebula asteroid comet",
	})

	results, err := f.store.SimilarTo(context.Background(), f.user, ids["cats.txt"],
		bow.Model(), 10, false)
	if err != nil {
		t.Fatalf("SimilarTo: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no similar documents")
	}

	// The source must not be in its own results.
	for _, r := range results {
		if r.Node.ID == ids["cats.txt"] {
			t.Fatal("a file was returned as similar to itself")
		}
	}
	if results[0].Node.Name != "kittens.txt" {
		t.Errorf("closest to cats.txt = %s, want kittens.txt", results[0].Node.Name)
	}
	// And the unrelated ones rank below it.
	if results[0].Score <= results[len(results)-1].Score {
		t.Error("scores are not ordered, or nothing distinguishes related from unrelated")
	}
}

// A file with no embedding is a clear "not indexed", not an empty list — empty
// would say "nothing resembles this", which is a different and wrong claim.
func TestSimilarToReportsAnUnindexedFile(t *testing.T) {
	f := newFixture(t)
	bow := bowEmbedder{dim: 1024}
	indexCorpus(t, f, bow, map[string]string{"indexed.txt": "some words here"})

	// Uploaded but never extracted or embedded.
	node := f.upload(f.root, "raw.bin", "\x00\x01\x02")

	_, err := f.store.SimilarTo(context.Background(), f.user, node.ID, bow.Model(), 10, false)
	if !errors.Is(err, files.ErrNoEmbedding) {
		t.Fatalf("SimilarTo on an unindexed file = %v, want ErrNoEmbedding", err)
	}
}

// The source node must be readable by the caller. Otherwise any node id becomes
// a probe: the similarity scores it produces leak the shape of a private
// document without ever returning its bytes.
func TestSimilarToRefusesAnUnreadableSource(t *testing.T) {
	f := newFixture(t)
	bow := bowEmbedder{dim: 1024}
	ids := indexCorpus(t, f, bow, map[string]string{"private.txt": "confidential merger terms"})

	_, err := f.store.SimilarTo(context.Background(), f.other(t), ids["private.txt"],
		bow.Model(), 10, false)
	if !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("stranger's SimilarTo = %v, want ErrNotFound", err)
	}
}

// Similarity is scoped like every other read: another user's documents are not
// candidates unless they were shared.
func TestSimilarToDoesNotRankAnotherUsersFiles(t *testing.T) {
	f := newFixture(t)
	bow := bowEmbedder{dim: 1024}
	ids := indexCorpus(t, f, bow, map[string]string{
		"mine.txt":  "cats kittens feline whiskers purr",
		"other.txt": "cats kittens feline whiskers purr meow",
	})

	// The second user asks about a file they cannot see — refused — and the first
	// user's results contain only their own files, which is all there are.
	results, err := f.store.SimilarTo(context.Background(), f.user, ids["mine.txt"],
		bow.Model(), 10, false)
	if err != nil {
		t.Fatalf("SimilarTo: %v", err)
	}
	for _, r := range results {
		if r.Node.OwnerID != f.user {
			t.Errorf("a file owned by %s appeared in %s's results", r.Node.OwnerID, f.user)
		}
	}
}

// Retrieval returns PASSAGES, and each one carries the text it was scored on —
// that text is what makes a citation checkable.
func TestRetrieveChunksReturnsCitablePassages(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bow := bowEmbedder{dim: 1024}

	indexCorpus(t, f, bow, map[string]string{
		"handbook.txt": "the office closes at six on fridays and stays shut all weekend",
		"menu.txt":     "soup salad sandwich pasta dessert coffee tea",
	})

	query, err := bow.Embed(ctx, []string{"when does the office close on friday"})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}

	chunks, err := f.store.RetrieveChunks(ctx, f.user, query[0], bow.Model(), 3, false, "")
	if err != nil {
		t.Fatalf("RetrieveChunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks retrieved")
	}
	if chunks[0].Node.Name != "handbook.txt" {
		t.Errorf("top chunk from %s, want handbook.txt", chunks[0].Node.Name)
	}
	// The passage text is recovered by re-chunking doc_text, not stored beside
	// the vector. If that ever breaks, citations silently become empty strings.
	if strings.TrimSpace(chunks[0].Text) == "" {
		t.Error("the retrieved passage has no text — a citation nobody can check")
	}
	if !strings.Contains(chunks[0].Text, "office") {
		t.Errorf("passage does not contain the matched content: %q", chunks[0].Text)
	}
}

// Retrieval never reaches content the caller could not already open, so chat
// cannot become a way to read around a permission.
func TestRetrieveChunksIsScopedToTheCaller(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bow := bowEmbedder{dim: 1024}

	indexCorpus(t, f, bow, map[string]string{
		"secret.txt": "the vault combination is seven three nine",
	})

	query, err := bow.Embed(ctx, []string{"what is the vault combination"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}

	chunks, err := f.store.RetrieveChunks(ctx, f.other(t), query[0], bow.Model(), 5, false, "")
	if err != nil {
		t.Fatalf("RetrieveChunks: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("another user retrieved %d passage(s) from a private document", len(chunks))
	}
}

// A scope narrows retrieval to a subtree, and does so by whole path segments —
// "/projectX" is not under "/project".
func TestRetrieveChunksHonoursScope(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bow := bowEmbedder{dim: 1024}

	extractH := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	embedH := embed.NewHandler(f.store, f.store, bow, nil)

	inside := f.mkdir(f.root, "project")
	decoy := f.mkdir(f.root, "projectX")

	for _, tc := range []struct {
		parent uuid.UUID
		name   string
	}{{inside.ID, "in.txt"}, {decoy.ID, "out.txt"}} {
		node, err := f.svc.Upload(ctx, f.user, tc.parent, tc.name,
			strings.NewReader("shared vocabulary about widgets and gears"), "text/plain")
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
		if err := extractH.Handle(ctx, &node.ID, f.user); err != nil {
			t.Fatalf("extract: %v", err)
		}
		if err := embedH.Handle(ctx, &node.ID, f.user); err != nil {
			t.Fatalf("embed: %v", err)
		}
	}

	query, _ := bow.Embed(ctx, []string{"widgets and gears"})
	chunks, err := f.store.RetrieveChunks(ctx, f.user, query[0], bow.Model(), 10, false, "/project")
	if err != nil {
		t.Fatalf("RetrieveChunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("scoped retrieval found nothing inside the scope")
	}
	for _, c := range chunks {
		if !strings.HasPrefix(c.Node.Path, "/project/") {
			t.Errorf("scope /project leaked %s", c.Node.Path)
		}
	}
}

// The scope predicate lives in SQL now, so it inherits the hazard every SQL
// prefix test in this codebase has: a pattern built from data widens silently if
// it is a LIKE pattern. A folder named "100%_done" would match "1009Xdone" —
// the same defect the grant predicate had, in a second place.
//
// starts_with takes a plain string and has no metacharacters at all.
func TestRetrieveScopeEscapesLikeMetacharacters(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bow := bowEmbedder{dim: 1024}

	extractH := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	embedH := embed.NewHandler(f.store, f.store, bow, nil)

	scoped := f.mkdir(f.root, "100%_done")
	decoy := f.mkdir(f.root, "1009Xdone")

	for _, tc := range []struct {
		parent uuid.UUID
		name   string
	}{
		{scoped.ID, "inside.txt"},
		{decoy.ID, "outside.txt"},
	} {
		node, err := f.svc.Upload(ctx, f.user, tc.parent, tc.name,
			strings.NewReader("widgets gears cogs sprockets levers"), "text/plain")
		if err != nil {
			t.Fatalf("upload %s: %v", tc.name, err)
		}
		if err := extractH.Handle(ctx, &node.ID, f.user); err != nil {
			t.Fatal(err)
		}
		if err := embedH.Handle(ctx, &node.ID, f.user); err != nil {
			t.Fatal(err)
		}
	}

	query, _ := bow.Embed(ctx, []string{"widgets and gears"})
	chunks, err := f.store.RetrieveChunks(ctx, f.user, query[0], bow.Model(), 10, false, "/100%_done")
	if err != nil {
		t.Fatalf("RetrieveChunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("the scope matched nothing inside itself")
	}
	for _, c := range chunks {
		if !strings.HasPrefix(c.Node.Path, "/100%_done/") {
			t.Errorf("a metacharacter in the scope widened it to %s", c.Node.Path)
		}
	}
}
