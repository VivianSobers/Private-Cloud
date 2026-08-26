package files_test

import (
	"context"
	"strings"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// pgvectorAvailable reports whether the test database took migration 00026's
// optional half. It is a skip condition rather than a failure: the migration is
// written to succeed on stock Postgres precisely so a database without the
// extension is a supported deployment, and CI runs both.
func pgvectorAvailable(t *testing.T, f *fixture) bool {
	t.Helper()
	var ok bool
	err := f.store.Pool().QueryRow(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'doc_embedding' AND column_name = 'vec'
		)`).Scan(&ok)
	if err != nil {
		t.Fatalf("probe pgvector column: %v", err)
	}
	return ok
}

// The property the whole pgvector path stands on: it must rank the same corpus
// the same way the exact scan does.
//
// The two paths compute similarity in different places — Go over unpacked
// float32 on one side, Postgres over a vector column on the other — so this is
// the test that catches them drifting on wire format, on cosine-versus-distance,
// or on which chunk of a document represents it. Ranking is compared rather than
// raw scores, because ranking is what a caller sees.
func TestPgvectorRanksLikeTheExactScan(t *testing.T) {
	f := newFixture(t)
	if !pgvectorAvailable(t, f) {
		t.Skip("pgvector not installed on the test database")
	}
	ctx := context.Background()
	bow := bowEmbedder{dim: 256}

	extractH := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	embedH := embed.NewHandler(f.store, f.store, bow, nil)

	docs := map[string]string{
		"cats.txt":    "cats kittens feline whiskers purr meow tabby claws",
		"finance.txt": "invoice payment tax revenue budget accounting ledger audit",
		"space.txt":   "planets orbit galaxy stars cosmos nebula asteroid comet",
		"mixed.txt":   "cats invoice galaxy whiskers budget orbit",
	}
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
	}

	query, err := bow.Embed(ctx, []string{"feline kittens purr whiskers"})
	if err != nil {
		t.Fatal(err)
	}

	// The write path fills `vec` as it goes, so nothing is pending and the
	// indexed path is live. This is the ranking a real caller gets.
	indexed, err := f.store.SemanticSearch(ctx, f.user, query[0], bow.Model(), 10, false)
	if err != nil {
		t.Fatalf("indexed search: %v", err)
	}
	if len(indexed) == 0 {
		t.Fatal("indexed search returned nothing")
	}

	// Blank every vec to force the fallback, then compare. Emptying the column
	// is exactly the state the pending guard exists to detect, so this also
	// proves the guard sends the query to the scan rather than silently
	// returning a short page.
	if _, err := f.store.Pool().Exec(ctx, `UPDATE doc_embedding SET vec = NULL WHERE model = $1`, bow.Model()); err != nil {
		t.Fatalf("blank vec: %v", err)
	}
	scanned, err := f.store.SemanticSearch(ctx, f.user, query[0], bow.Model(), 10, false)
	if err != nil {
		t.Fatalf("scan search: %v", err)
	}

	if len(indexed) != len(scanned) {
		t.Fatalf("indexed returned %d results, scan returned %d", len(indexed), len(scanned))
	}
	for i := range indexed {
		if indexed[i].Node.ID != scanned[i].Node.ID {
			t.Errorf("rank %d: indexed has %s, scan has %s",
				i, indexed[i].Node.Name, scanned[i].Node.Name)
		}
		// Same similarity, computed two ways. float32 storage on both sides
		// means the difference should be far below anything that could reorder
		// a page.
		if d := indexed[i].Score - scanned[i].Score; d > 1e-6 || d < -1e-6 {
			t.Errorf("rank %d (%s): indexed score %v, scan score %v",
				i, indexed[i].Node.Name, indexed[i].Score, scanned[i].Score)
		}
	}
}

// Proof that the indexed path is actually the one running, and that it ranks on
// `vec` while the fallback ranks on the packed bytes.
//
// Without this, the parity test above would pass just as happily if pgvector
// were never consulted and both calls fell through to the scan — identical
// results for the wrong reason. So this deliberately drives the two columns out
// of agreement: one document's `vec` is overwritten with the query vector
// itself, which no honest embedding of its text would produce. The indexed path
// must then rank that document first, and the scan must still rank it nowhere
// near, because they are reading different columns.
func TestPgvectorPathRanksOnTheVectorColumn(t *testing.T) {
	f := newFixture(t)
	if !pgvectorAvailable(t, f) {
		t.Skip("pgvector not installed on the test database")
	}
	ctx := context.Background()
	bow := bowEmbedder{dim: 128}

	extractH := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	embedH := embed.NewHandler(f.store, f.store, bow, nil)

	upload := func(name, body string) *files.Node {
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
		return node
	}

	cats := upload("cats.txt", "cats kittens feline whiskers purr meow tabby claws")
	finance := upload("finance.txt", "invoice payment tax revenue budget accounting ledger audit")

	query, err := bow.Embed(ctx, []string{"feline kittens purr whiskers"})
	if err != nil {
		t.Fatal(err)
	}

	// finance.txt's pgvector copy now claims to be exactly the query. Its packed
	// bytes are untouched and still describe an accounting document.
	if _, err := f.store.Pool().Exec(ctx,
		`UPDATE doc_embedding SET vec = $2::vector WHERE content_hash = $1 AND model = $3`,
		finance.ContentHash, embed.Literal(query[0]), bow.Model()); err != nil {
		t.Fatalf("rewrite vec: %v", err)
	}

	indexed, err := f.store.SemanticSearch(ctx, f.user, query[0], bow.Model(), 10, false)
	if err != nil {
		t.Fatalf("indexed search: %v", err)
	}
	if len(indexed) == 0 {
		t.Fatal("indexed search returned nothing")
	}
	if indexed[0].Node.ID != finance.ID {
		t.Fatalf("top indexed result is %s; the pgvector column was not what ranked this, "+
			"so the query silently fell back to the exact scan", indexed[0].Node.Name)
	}

	// And the scan, reading the bytes, still knows finance.txt is about invoices.
	if _, err := f.store.Pool().Exec(ctx, `UPDATE doc_embedding SET vec = NULL WHERE model = $1`, bow.Model()); err != nil {
		t.Fatalf("blank vec: %v", err)
	}
	scanned, err := f.store.SemanticSearch(ctx, f.user, query[0], bow.Model(), 10, false)
	if err != nil {
		t.Fatalf("scan search: %v", err)
	}
	if len(scanned) == 0 || scanned[0].Node.ID != cats.ID {
		t.Fatalf("scan ranked %v first, want cats.txt", scanned[0].Node.Name)
	}
}

// A grantee's page is filled from rows they may see, not truncated to whatever
// the nearest vectors happened to be.
//
// This is the failure an ANN index invites and the reason the ACL filter stays
// on the node rows: the owner's documents are the nearest neighbours by
// construction here, so an implementation that took the top-N vectors first and
// filtered afterwards would hand the grantee an empty result for a document they
// were explicitly given.
func TestPgvectorFillsThePageThroughTheACLFilter(t *testing.T) {
	f := newFixture(t)
	if !pgvectorAvailable(t, f) {
		t.Skip("pgvector not installed on the test database")
	}
	ctx := context.Background()
	bow := bowEmbedder{dim: 64}

	extractH := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	embedH := embed.NewHandler(f.store, f.store, bow, nil)

	// A crowd of the owner's own near-identical cat documents, then one shared
	// with somebody else. The crowd is what a naive top-N would return in full.
	for _, name := range []string{"c1.txt", "c2.txt", "c3.txt", "c4.txt", "c5.txt"} {
		node, err := f.svc.Upload(ctx, f.user, f.root, name,
			strings.NewReader("cats kittens feline whiskers purr meow"), "text/plain")
		if err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
		if err := extractH.Handle(ctx, &node.ID, f.user); err != nil {
			t.Fatalf("extract %s: %v", name, err)
		}
		if err := embedH.Handle(ctx, &node.ID, f.user); err != nil {
			t.Fatalf("embed %s: %v", name, err)
		}
	}

	shared, err := f.svc.Upload(ctx, f.user, f.root, "shared-cats.txt",
		strings.NewReader("cats kittens feline whiskers purr tabby"), "text/plain")
	if err != nil {
		t.Fatalf("upload shared: %v", err)
	}
	if err := extractH.Handle(ctx, &shared.ID, f.user); err != nil {
		t.Fatalf("extract shared: %v", err)
	}
	if err := embedH.Handle(ctx, &shared.ID, f.user); err != nil {
		t.Fatalf("embed shared: %v", err)
	}

	other := f.other(t)
	if _, err := f.store.CreateGrant(ctx, f.user, shared.ID, other, files.RoleViewer); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	query, err := bow.Embed(ctx, []string{"feline kittens purr whiskers"})
	if err != nil {
		t.Fatal(err)
	}

	results, err := f.store.SemanticSearch(ctx, other, query[0], bow.Model(), 10, true)
	if err != nil {
		t.Fatalf("grantee search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("grantee got %d results, want exactly the 1 shared document", len(results))
	}
	if results[0].Node.ID != shared.ID {
		t.Errorf("grantee saw %s, want shared-cats.txt", results[0].Node.Name)
	}
}
