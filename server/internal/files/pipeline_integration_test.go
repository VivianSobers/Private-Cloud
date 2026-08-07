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

// Re-indexing enqueues an extract job for every live file, and is idempotent: a
// second run creates nothing new because the first run's jobs are still pending.
func TestReindexEnqueuesLiveFiles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	jobStore := jobs.NewStore(f.store.Pool())

	var ids []uuid.UUID
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		n, err := f.svc.Upload(ctx, f.user, f.root, name, strings.NewReader("body"), "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
	}

	if _, err := jobStore.EnqueueForAllFiles(ctx, extract.Kind); err != nil {
		t.Fatal(err)
	}

	// Each of this user's files now has exactly one pending extract job.
	pending := func() int {
		var n int
		if err := f.store.Pool().QueryRow(ctx, `
			SELECT count(*) FROM jobs
			WHERE kind = $1 AND node_id = ANY($2) AND state IN ('queued','running')`,
			extract.Kind, ids).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := pending(); got != len(ids) {
		t.Fatalf("pending extract jobs = %d, want %d", got, len(ids))
	}

	// A second reindex is a no-op for these files — the unique-pending index dedups.
	if _, err := jobStore.EnqueueForAllFiles(ctx, extract.Kind); err != nil {
		t.Fatal(err)
	}
	if got := pending(); got != len(ids) {
		t.Errorf("after second reindex, pending = %d, want %d (no duplicates)", got, len(ids))
	}
}

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

	// Drain the queue. Only THIS test's node is handled and asserted; any foreign
	// extract jobs from sibling tests sharing the database (whose content lives in
	// a different, already-cleaned-up blob store) are just cleared out of the way.
	var handled bool
	for i := 0; i < 200; i++ {
		job, err := jobStore.Claim(ctx, []string{extract.Kind})
		if errors.Is(err, jobs.ErrNoJob) {
			break
		}
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if job.NodeID != nil && *job.NodeID == node.ID {
			if err := handler.Handle(ctx, job.NodeID, job.OwnerID); err != nil {
				t.Fatalf("handle: %v", err)
			}
			handled = true
		}
		if err := jobStore.Complete(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	if !handled {
		t.Fatal("upload did not enqueue a claimable extract job for this file")
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
