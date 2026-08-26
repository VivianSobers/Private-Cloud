package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Phase 8, slice 5: the image space, and the routing between the two.
//
// The invariants under test are the ones the phase's decision table names, and
// every one of them predates this space — which is the point. Adding a second
// vector store must not weaken /similar's access rule, must not let a file be
// its own neighbour, must not turn an unindexed file into an empty list, and
// must not take away the text-space answer a file already had.

// clipEmbedder is a deterministic stand-in for a vision encoder: the vector is
// derived from the bytes, so two "photographs" that share a visual token rank
// together and an unrelated one does not. No GPU, no sidecar, no network.
type clipEmbedder struct{ dim int }

func (c clipEmbedder) Model() string { return "fake-clip-" + fmt.Sprint(c.dim) }
func (c clipEmbedder) Dim() int      { return c.dim }

func (c clipEmbedder) EmbedImage(_ context.Context, _ string, data []byte) ([]float32, error) {
	v := make([]float32, c.dim)
	// A bag of bytes, exactly as bowEmbedder is a bag of words: content that
	// shares bytes scores high, content that does not scores low.
	for _, b := range data {
		v[int(b)%c.dim] += 1
	}
	return v, nil
}

// newAPIFixtureWithImages wires both spaces so the routing has something to
// choose between.
func newAPIFixtureWithImages(t *testing.T, withText bool) (*apiFixture, clipEmbedder) {
	t.Helper()
	f := newAPIFixture(t)
	clip := clipEmbedder{dim: 64}
	if withText {
		f.srv.SetEmbedder(bowEmbedder{dim: 1024})
	}
	f.srv.SetImageEmbedder(clip)
	return f, clip
}

// indexImage uploads bytes as a photograph and runs the real image-embedding
// handler over it — the same handler pcworker registers, so the test exercises
// the job rather than a hand-written row.
func (f *apiFixture) indexImage(t *testing.T, clip clipEmbedder, name, body string) uuid.UUID {
	t.Helper()
	return f.indexImageFor(t, clip, f.userID, name, body)
}

// indexImageFor is the same for any owner, so the ACL tests can put a second
// user's photograph in the table — which is where the interesting failure is:
// the vector row is content-addressed and shared by construction, so only the
// node filter keeps the two libraries apart.
func (f *apiFixture) indexImageFor(t *testing.T, clip clipEmbedder, owner uuid.UUID, name, body string) uuid.UUID {
	t.Helper()

	root, err := f.store.EnsureRoot(f.ctx, owner)
	if err != nil {
		t.Fatalf("ensure root: %v", err)
	}
	node, err := f.filesSvc.Upload(f.ctx, owner, root.ID, name,
		strings.NewReader(body), "image/jpeg")
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	h := embed.NewImageHandler(
		files.NewImageEmbedOpener(f.filesSvc), files.NewImageVectorStore(f.store), clip, nil)
	if err := h.Handle(f.ctx, &node.ID, owner); err != nil {
		t.Fatalf("embed image %s: %v", name, err)
	}
	return node.ID
}

// adminUserID resolves the fixture's second account, which owns nothing the
// first can see. Read from the table rather than carried on the fixture so this
// file adds no field to a struct other tests share.
func (f *apiFixture) adminUserID(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM users WHERE username = $1`, f.adminUsername).Scan(&id); err != nil {
		t.Fatalf("resolve the second account: %v", err)
	}
	return id
}

// A photograph with no text now has neighbours, and the response says which
// space ranked them — the two claims slice 5 exists to make.
func TestSimilarRanksPhotographsInTheImageSpace(t *testing.T) {
	f, clip := newAPIFixtureWithImages(t, true)

	source := f.indexImage(t, clip, "beach.jpg", "aaaaaaaaaabbbbbbbbbb")
	near := f.indexImage(t, clip, "beach2.jpg", "aaaaaaaaaabbbbbbbbbc")
	far := f.indexImage(t, clip, "ledger.jpg", "zzzzzzzzzzyyyyyyyyyy")

	rec := f.do(http.MethodGet, "/api/v1/nodes/"+source.String()+"/similar", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("similar on a photograph = %d, want 200", rec.Code)
	}
	body := decode(t, rec)
	if body["space"] != files.SpaceImage {
		t.Fatalf("ranked in space %v, want %q", body["space"], files.SpaceImage)
	}

	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatal("a photograph with an image vector still has no neighbours")
	}
	ids := make([]string, 0, len(results))
	for _, r := range results {
		item, _ := r.(map[string]any)
		id, _ := item["id"].(string)
		ids = append(ids, id)
		// A file is excluded from its own results — the invariant the decision
		// table names, unchanged by the space it ranks in.
		if id == source.String() {
			t.Error("a photograph was returned as similar to itself")
		}
	}
	if len(ids) == 0 || ids[0] != near.String() {
		t.Errorf("closest to beach.jpg = %v, want %s", ids, near)
	}
	if ids[len(ids)-1] == near.String() && len(ids) > 1 {
		t.Error("the unrelated photograph did not rank below the related one")
	}
	_ = far
}

// The regression this slice must not cause: a file with no IMAGE vector but with
// text neighbours keeps answering exactly as it did before the space existed.
func TestSimilarFallsBackToTheTextSpace(t *testing.T) {
	f, _ := newAPIFixtureWithImages(t, true)

	f.indexDoc(t, "cats.txt", "cats kittens feline whiskers purr meow tabby claws")
	f.indexDoc(t, "kittens.txt", "kittens feline purr whiskers playful meow paws")

	node, err := f.store.GetByPath(f.ctx, f.userID, "/cats.txt")
	if err != nil {
		t.Fatalf("resolve cats.txt: %v", err)
	}

	rec := f.do(http.MethodGet, "/api/v1/nodes/"+node.ID.String()+"/similar", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("similar on a document = %d, want 200", rec.Code)
	}
	body := decode(t, rec)
	if body["space"] != files.SpaceText {
		t.Fatalf("a document ranked in space %v, want %q", body["space"], files.SpaceText)
	}
	if results, _ := body["results"].([]any); len(results) == 0 {
		t.Error("configuring the image space took away a document's text neighbours")
	}
}

// An unindexed file is still 404 not_indexed. Empty would claim "nothing
// resembles this", which is a different and wrong claim, and 503 would tell the
// client to retry a feature that is working fine.
func TestSimilarStillReports404ForAnUnindexedFile(t *testing.T) {
	f, _ := newAPIFixtureWithImages(t, true)

	node, err := f.filesSvc.Upload(f.ctx, f.userID, f.rootID(t), "raw.bin",
		strings.NewReader("\x00\x01\x02"), "application/octet-stream")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	rec := f.do(http.MethodGet, "/api/v1/nodes/"+node.ID.String()+"/similar", nil, f.cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("similar on an unindexed file = %d, want 404", rec.Code)
	}
	code, _ := decode(t, rec)["error"].(map[string]any)["code"].(string)
	if code != "not_indexed" {
		t.Errorf("error code = %q, want not_indexed", code)
	}
}

// The ACL invariant, in the space that did not exist when it was written.
//
// /similar requires read on the SOURCE, and the routing decision — "is this node
// in the image space?" — is made after that check rather than before it. If it
// were made first, the answer to it would leak for a node the caller may not
// know exists, which is the probe the read requirement closes.
func TestSimilarInTheImageSpaceRefusesAnUnreadableSource(t *testing.T) {
	f, clip := newAPIFixtureWithImages(t, true)
	source := f.indexImage(t, clip, "private.jpg", "aaaaaaaaaabbbbbbbbbb")

	rec := f.do(http.MethodGet, "/api/v1/nodes/"+source.String()+"/similar", nil, f.admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger's similar on a private photograph = %d, want 404", rec.Code)
	}
}

// And the other half: an image vector is content-addressed and therefore shared
// between two owners of the same picture by construction, so the filter has to
// be on the NODE rows. A stranger's own photograph must never rank another
// user's, however close the two vectors are.
func TestSimilarInTheImageSpaceDoesNotRankAnotherUsersPhotos(t *testing.T) {
	f, clip := newAPIFixtureWithImages(t, true)
	source := f.indexImage(t, clip, "mine.jpg", "aaaaaaaaaabbbbbbbbbb")
	f.indexImage(t, clip, "alsomine.jpg", "aaaaaaaaaabbbbbbbbbc")
	// The stranger's photograph is byte-for-byte the nearest thing in the table,
	// so if the filter were on the vectors rather than on the node rows this is
	// the row that would come back first.
	f.indexImageFor(t, clip, f.adminUserID(t), "theirs.jpg", "aaaaaaaaaabbbbbbbbbb")

	rec := f.do(http.MethodGet, "/api/v1/nodes/"+source.String()+"/similar", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("similar = %d, want 200", rec.Code)
	}
	for _, r := range decode(t, rec)["results"].([]any) {
		item, _ := r.(map[string]any)
		if name, _ := item["name"].(string); name == "theirs.jpg" {
			t.Error("another user's photograph ranked in this caller's results")
		}
	}
}

// With no sidecar of either kind the endpoint reports itself unavailable rather
// than erroring — the phase's rule, and the behaviour a client already handles
// from /search?semantic=true.
func TestSimilarUnavailableWithoutAnySpace(t *testing.T) {
	f := newAPIFixture(t)
	node, err := f.filesSvc.Upload(f.ctx, f.userID, f.rootID(t), "photo.jpg",
		strings.NewReader("bytes"), "image/jpeg")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	rec := f.do(http.MethodGet, "/api/v1/nodes/"+node.ID.String()+"/similar", nil, f.cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("similar with no space configured = %d, want 503", rec.Code)
	}
}

// A server running ONLY the image sidecar is a coherent deployment: photographs
// rank, and a text file reports not_indexed — which is what it would have
// reported on a server with no space at all, only now with a working feature
// beside it.
func TestSimilarWorksWithTheImageSpaceAlone(t *testing.T) {
	f, clip := newAPIFixtureWithImages(t, false)
	source := f.indexImage(t, clip, "solo.jpg", "aaaaaaaaaabbbbbbbbbb")
	f.indexImage(t, clip, "solo2.jpg", "aaaaaaaaaabbbbbbbbbc")

	rec := f.do(http.MethodGet, "/api/v1/nodes/"+source.String()+"/similar", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("image-only similar = %d, want 200", rec.Code)
	}
	if body := decode(t, rec); body["space"] != files.SpaceImage {
		t.Errorf("space = %v, want %q", body["space"], files.SpaceImage)
	}
}
