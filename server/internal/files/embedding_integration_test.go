package files_test

import (
	"context"
	"hash/fnv"
	"strconv"
	"strings"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// bowEmbedder is a deterministic bag-of-words embedder: it hashes each word into
// a bucket of a fixed-dimension vector. It is not a real model, but it has the
// one property the pipeline test needs — texts that share words get similar
// vectors — so it exercises storage and cosine ranking without a sidecar.
type bowEmbedder struct{ dim int }

// Model encodes the dimension, as a real model's identity does: two widths are
// two models, never the same one with mismatched vectors.
func (b bowEmbedder) Model() string { return "bow-test-" + strconv.Itoa(b.dim) }
func (b bowEmbedder) Dim() int      { return b.dim }
func (b bowEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, b.dim)
		for _, tok := range strings.Fields(strings.ToLower(t)) {
			h := fnv.New32a()
			h.Write([]byte(tok))
			v[int(h.Sum32())%b.dim]++
		}
		out[i] = v
	}
	return out, nil
}

// The slice-3 payoff: a file is found by MEANING. Three documents on distinct
// topics are extracted and embedded; a query about kittens ranks the cats file
// first, though it shares no exact query word with the filename.
func TestSemanticSearchRanksByMeaning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A wide vector so hashing collisions among the corpus's few dozen distinct
	// words are negligible — the fake then ranks by genuine word overlap.
	bow := bowEmbedder{dim: 1024}

	extractH := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	embedH := embed.NewHandler(f.store, f.store, bow, nil)

	docs := map[string]string{
		"cats.txt":    "cats kittens feline whiskers purr meow tabby claws",
		"finance.txt": "invoice payment tax revenue budget accounting ledger audit",
		"space.txt":   "planets orbit galaxy stars cosmos nebula asteroid comet",
	}
	ids := map[string]string{}
	for name, body := range docs {
		node, err := f.svc.Upload(ctx, f.user, f.root, name, strings.NewReader(body), "text/plain")
		if err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
		ids[name] = node.ID.String()
		if err := extractH.Handle(ctx, &node.ID, f.user); err != nil {
			t.Fatalf("extract %s: %v", name, err)
		}
		if err := embedH.Handle(ctx, &node.ID, f.user); err != nil {
			t.Fatalf("embed %s: %v", name, err)
		}
		// Embedding is content-addressed and idempotent.
		if has, _ := f.store.HasEmbedding(ctx, node.ContentHash, bow.Model()); !has {
			t.Fatalf("embedding not stored for %s", name)
		}
	}

	// Words drawn from the cats document, so genuine overlap — not collision noise
	// — makes it the match. A real embedding model would also match "kitten pet",
	// but that is the model's job to test, not this pipeline's.
	query, err := bow.Embed(ctx, []string{"feline kittens purr whiskers"})
	if err != nil {
		t.Fatal(err)
	}
	results, err := f.store.SemanticSearch(ctx, f.user, query[0], bow.Model(), 10, false)
	if err != nil {
		t.Fatalf("semantic search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no semantic results")
	}
	if results[0].Node.ID.String() != ids["cats.txt"] {
		t.Errorf("top result is %s, want cats.txt", results[0].Node.Name)
	}
	if !results[0].Semantic || results[0].Score <= 0 {
		t.Errorf("result not flagged semantic with a positive score: %+v", results[0])
	}
}

// A model the corpus was not embedded with returns nothing, rather than comparing
// across incompatible vector spaces.
func TestSemanticSearchScopedToModel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bow := bowEmbedder{dim: 64}

	extractH := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	embedH := embed.NewHandler(f.store, f.store, bow, nil)

	node, err := f.svc.Upload(ctx, f.user, f.root, "doc.txt", strings.NewReader("hello semantic world"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := extractH.Handle(ctx, &node.ID, f.user); err != nil {
		t.Fatal(err)
	}
	if err := embedH.Handle(ctx, &node.ID, f.user); err != nil {
		t.Fatal(err)
	}

	query, _ := bow.Embed(ctx, []string{"hello world"})
	// Correct model finds it.
	if res, err := f.store.SemanticSearch(ctx, f.user, query[0], bow.Model(), 10, false); err != nil || len(res) == 0 {
		t.Fatalf("same-model search found nothing: %d err=%v", len(res), err)
	}
	// A different model's space has no vectors here.
	if res, err := f.store.SemanticSearch(ctx, f.user, query[0], "other-model", 10, false); err != nil || len(res) != 0 {
		t.Errorf("cross-model search returned %d results, want 0 (err=%v)", len(res), err)
	}
}
