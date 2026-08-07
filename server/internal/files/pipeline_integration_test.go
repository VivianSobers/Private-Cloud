package files_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs"
)

// pipelineEnqueuer is the real enqueue path the API uses, pointed at a jobs store.
type pipelineEnqueuer struct{ store *jobs.Store }

func (e pipelineEnqueuer) EnqueueExtract(ctx context.Context, nodeID, ownerID uuid.UUID) {
	_, _, _ = e.store.Enqueue(ctx, extract.Kind, &nodeID, ownerID, jobs.EnqueueOptions{})
}

// The whole Phase 4 extraction pipeline, end to end: uploading a file enqueues a
// real job; a worker claims it, runs the real extract handler, and stores the
// text and tags; the file is then findable by a word from its body. Each piece is
// unit-tested elsewhere — this proves they connect.
func TestExtractionPipelineEndToEnd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	jobStore := jobs.NewStore(f.store.Pool())
	f.svc.SetEnqueuer(pipelineEnqueuer{store: jobStore})

	// A token only in the body, so a hit can only come from extracted text.
	token := "zebra" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	node, err := f.svc.Upload(ctx, f.user, f.root, "memo.txt",
		strings.NewReader("Project kickoff. Codename "+token+". Invoice attached."), "text/plain")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	// The upload enqueued an extract job. Build the worker's real handler and drain
	// the queue — completing any stale extract jobs from earlier runs along the way
	// (their content is gone, so the handler no-ops them).
	handler := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	handler.Tagging(f.store)

	var drained int
	for i := 0; i < 100; i++ {
		job, err := jobStore.Claim(ctx, []string{extract.Kind})
		if errors.Is(err, jobs.ErrNoJob) {
			break
		}
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := handler.Handle(ctx, job.NodeID, job.OwnerID); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if err := jobStore.Complete(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
		drained++
	}
	if drained == 0 {
		t.Fatal("upload did not enqueue an extract job")
	}

	// The text is now cached and the file is searchable by its body word.
	if _, ok, err := f.store.DocText(ctx, node.ContentHash); err != nil || !ok {
		t.Fatalf("pipeline did not store doc text: ok=%v err=%v", ok, err)
	}
	results, err := f.store.Search(ctx, f.user, files.SearchQuery{Text: token})
	if err != nil {
		t.Fatal(err)
	}
	var hit *files.SearchResult
	for _, r := range results {
		if r.Node.ID == node.ID {
			hit = r
		}
	}
	if hit == nil {
		t.Fatalf("file not found by its content after the pipeline ran")
	}
	if !hit.MatchedContent {
		t.Error("result not flagged as a content match")
	}

	// And the pipeline tagged it — a MIME tag at least, plus a keyword tag from
	// "invoice" in the body.
	tags, err := f.store.TagsForNode(ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(tags))
	for i, tg := range tags {
		names[i] = tg.Name
	}
	if !slices.Contains(names, "text") || !slices.Contains(names, "invoice") {
		t.Errorf("pipeline tags = %v, want to contain text + invoice", names)
	}
}
