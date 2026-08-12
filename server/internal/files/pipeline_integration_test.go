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
	"github.com/guru-bharadwaj20/private-cloud/server/internal/media"
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

func (e pipelineEnqueuer) EnqueueMedia(ctx context.Context, nodeID, ownerID uuid.UUID) {
	_, _, _ = e.store.Enqueue(ctx, media.Kind, &nodeID, ownerID, jobs.EnqueueOptions{})
}

// recordingEnqueuer captures which nodes were scheduled for which kind of
// derived work, without needing a jobs table.
type recordingEnqueuer struct {
	extract map[uuid.UUID]bool
	media   map[uuid.UUID]bool
}

func newRecordingEnqueuer() *recordingEnqueuer {
	return &recordingEnqueuer{extract: map[uuid.UUID]bool{}, media: map[uuid.UUID]bool{}}
}

func (e *recordingEnqueuer) EnqueueExtract(_ context.Context, nodeID, _ uuid.UUID) {
	e.extract[nodeID] = true
}

func (e *recordingEnqueuer) EnqueueMedia(_ context.Context, nodeID, _ uuid.UUID) {
	e.media[nodeID] = true
}

// TestEveryWritePathSchedulesDerivedWork covers all four ways a file gets into
// this system at once, because covering them one at a time is how one of them
// came to be missed.
//
// FinishStaged - the shared tail of resumable uploads and of every WebDAV write
// - never called scheduleExtract, on either of its storage branches. Everything
// arriving that way had no extracted text, no embedding, no EXIF, no thumbnail
// and no faces. It was invisible to content search, to /chat, to the timeline
// and to /people, and nothing anywhere reported it: the upload succeeded and the
// file looked entirely normal.
//
// The web client switches to resumable uploads at 8 MiB, so in practice this was
// "large files are not indexed", which is close to the worst possible split
// because large files are exactly the ones worth searching.
func TestEveryWritePathSchedulesDerivedWork(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	rec := newRecordingEnqueuer()
	f.svc.SetEnqueuer(rec)

	// 1. The simple POST path.
	direct, err := f.svc.Upload(ctx, f.user, f.root, "direct.txt",
		strings.NewReader("posted directly"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	// 2. The resumable path, which is also WebDAV's: both end in FinishStaged.
	body := "staged through the resumable protocol"
	sess, err := f.svc.CreateUpload(ctx, f.user, f.root, "staged.txt", int64(len(body)), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AppendChunk(ctx, f.user, sess.ID, 0, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	staged, err := f.svc.FinishUpload(ctx, f.user, sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path string
		node *files.Node
	}{
		{"POST /upload", direct},
		{"resumable finish / WebDAV write", staged},
	} {
		if !rec.extract[tc.node.ID] {
			t.Errorf("%s did not schedule text extraction for %s", tc.path, tc.node.Name)
		}
	}

	// A media file scheduled through the staged path gets media analysis too —
	// the enqueue-time MIME filter has to survive the same journey.
	png := string([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) + "not a real png body"
	sess, err = f.svc.CreateUpload(ctx, f.user, f.root, "photo.png", int64(len(png)), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AppendChunk(ctx, f.user, sess.ID, 0, strings.NewReader(png)); err != nil {
		t.Fatal(err)
	}
	photo, err := f.svc.FinishUpload(ctx, f.user, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.media[photo.ID] {
		t.Error("a staged image did not schedule media analysis")
	}
	if rec.media[staged.ID] {
		t.Error("a staged text file scheduled media analysis")
	}
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
	// The body has to look like an actual invoice, not merely mention the word.
	// The keyword vocabulary keys on phrases ("invoice number") rather than bare
	// words precisely so that "Invoice attached." in a memo does NOT get tagged.
	node, err := f.svc.Upload(ctx, f.user, f.root, "memo.txt",
		strings.NewReader("Project kickoff. Codename "+token+".\nInvoice number 4471\nAmount due: 42.00"),
		"text/plain")
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
