package files_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/media"
)

// The Phase 5 pipeline end to end: uploading an image enqueues a real media job;
// a worker claims it and runs the real handler; the metadata and both variants
// land; and the file then appears in the timeline with its variants listed.
//
// Each piece is unit-tested elsewhere. This is the test that would have caught
// the gap Phase 5 actually shipped with — a complete media package that nothing
// ever called.
func TestMediaPipelineEndToEnd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	jobStore := jobs.NewStore(f.store.Pool())
	f.svc.SetEnqueuer(pipelineEnqueuer{store: jobStore})

	// Large enough that both a thumb and a preview are worth rendering.
	node, err := f.svc.Upload(ctx, f.user, f.root, "photo.jpg",
		bytes.NewReader(testJPEG(t, 2000, 1500)), "image/jpeg")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	handler := media.NewHandler(
		files.NewMediaOpener(f.svc),
		files.NewMediaStore(f.store),
		files.NewMediaBlobWriter(f.svc),
		log,
	)

	// Drain the queue. Only THIS test's node is handled and asserted; sibling
	// tests share the database, and their nodes' bytes live in blob stores that
	// have already been cleaned up, so those jobs are only cleared out of the way.
	var handled bool
	for i := 0; i < 200; i++ {
		job, err := jobStore.Claim(ctx, []string{media.Kind})
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
		t.Fatal("uploading an image did not enqueue a claimable media job")
	}

	meta, ok, err := f.store.MediaMetaForNode(ctx, f.user, node.ID)
	if err != nil {
		t.Fatalf("media meta: %v", err)
	}
	if !ok {
		t.Fatal("no media metadata after the job ran — the pipeline is not connected")
	}
	if meta.Width != 2000 || meta.Height != 1500 {
		t.Errorf("dimensions = %dx%d, want 2000x1500", meta.Width, meta.Height)
	}
	if meta.Source != "image" {
		t.Errorf("source = %q, want image", meta.Source)
	}
	if len(meta.Variants) != 2 {
		t.Fatalf("variants = %v, want both thumb and preview", meta.Variants)
	}

	// The variant has to be readable through the same path the API serves it on,
	// scoped to the owner.
	v, rc, err := f.svc.OpenMediaVariant(ctx, f.user, node.ID, media.VariantThumb)
	if err != nil {
		t.Fatalf("open thumb: %v", err)
	}
	defer rc.Close()
	if v.Width > media.ThumbMaxEdge && v.Height > media.ThumbMaxEdge {
		t.Errorf("thumb is %dx%d, neither edge within %d", v.Width, v.Height, media.ThumbMaxEdge)
	}
	body, err := io.ReadAll(rc)
	if err != nil || len(body) == 0 {
		t.Fatalf("thumb bytes unreadable: %v (%d bytes)", err, len(body))
	}

	// And it is in the timeline, which is the gallery's actual read.
	items, err := f.store.TimelineNodes(ctx, f.user, nil, nil, 50, 0)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(items) != 1 || items[0].ID != node.ID {
		t.Fatalf("timeline returned %d item(s), want the uploaded photo", len(items))
	}
}

// A non-media upload must not enqueue a media job at all. The filter is at the
// enqueue rather than in the handler so a library of documents does not fill the
// queue with work that opens each file only to skip it.
func TestNonMediaUploadEnqueuesNoMediaJob(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	jobStore := jobs.NewStore(f.store.Pool())
	f.svc.SetEnqueuer(pipelineEnqueuer{store: jobStore})

	node, err := f.svc.Upload(ctx, f.user, f.root, "notes.txt",
		strings.NewReader("just words"), "text/plain")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	// Drain every media job in the shared database and assert none of them is
	// this node's. Checking "the queue is empty" would be wrong here — sibling
	// tests put their own media jobs in it.
	for i := 0; i < 200; i++ {
		job, err := jobStore.Claim(ctx, []string{media.Kind})
		if errors.Is(err, jobs.ErrNoJob) {
			break
		}
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if job.NodeID != nil && *job.NodeID == node.ID {
			t.Fatal("a text/plain upload enqueued a media job — the MIME filter is not applied")
		}
		if err := jobStore.Complete(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReRunRebuildsALostVariant is the repair path fsck sends operators to.
//
// FsckReport.MissingVariants tells an operator that missing thumbnails are not
// data loss because "re-running the media job rebuilds it", and names
// `cloudctl jobs reindex --kind=media` as the remedy. It was not a remedy: the
// handler returned as soon as it found a metadata row, so the enqueued job did
// precisely nothing and the thumbnail stayed missing for the life of the file.
//
// The state is easy to reach without any corruption. Variants are rendered after
// the metadata row and are deliberately best effort, so a full disk, a killed
// worker or one failed blob write leaves exactly this: analysed, not rendered.
func TestReRunRebuildsALostVariant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Deliberately not the same dimensions as any other test's image. testJPEG is
	// deterministic, so identical dimensions are identical bytes, and the whole
	// point of content addressing is that identical bytes share one media_variant
	// row — across fixtures whose blob stores are separate temp directories.
	node, err := f.svc.Upload(ctx, f.user, f.root, "repairable.jpg",
		bytes.NewReader(testJPEG(t, 1800, 1200)), "image/jpeg")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	handler := media.NewHandler(
		files.NewMediaOpener(f.svc),
		files.NewMediaStore(f.store),
		files.NewMediaBlobWriter(f.svc),
		log,
	)
	if err := handler.Handle(ctx, &node.ID, f.user); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Lose the thumbnail the way a failed render would have: the metadata row
	// stays, the variant row does not.
	if _, err := f.store.Pool().Exec(ctx,
		`DELETE FROM media_variant WHERE content_hash = $1 AND variant = $2`,
		node.ContentHash, media.VariantThumb); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.OpenMediaVariant(ctx, f.user, node.ID, media.VariantThumb); err == nil {
		t.Fatal("the thumbnail was not actually removed")
	}

	// Re-running the job is what a reindex does. It has to notice.
	if err := handler.Handle(ctx, &node.ID, f.user); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if _, rc, err := f.svc.OpenMediaVariant(ctx, f.user, node.ID, media.VariantThumb); err != nil {
		t.Errorf("re-running the media job did not rebuild the thumbnail: %v", err)
	} else {
		rc.Close()
	}

	// And the preview, which never went missing, was not re-encoded — Render is
	// asked only for the names that are absent.
	meta, ok, err := f.store.MediaMetaForNode(ctx, f.user, node.ID)
	if err != nil || !ok {
		t.Fatalf("media meta: ok=%v err=%v", ok, err)
	}
	if len(meta.Variants) != 2 {
		t.Errorf("variants after repair = %v, want both", meta.Variants)
	}
}

// TestSmallImageIsNotForeverUnfinished guards the other side of the same
// decision. An image already smaller than a thumbnail correctly has no variants,
// and a "missing variants" test that could not tell that apart from a failed
// render would re-open and re-decode every icon in the library on every pass.
func TestSmallImageIsNotForeverUnfinished(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	node, err := f.svc.Upload(ctx, f.user, f.root, "icon.jpg",
		bytes.NewReader(testJPEG(t, 64, 64)), "image/jpeg")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	handler := media.NewHandler(
		files.NewMediaOpener(f.svc),
		files.NewMediaStore(f.store),
		files.NewMediaBlobWriter(f.svc),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := handler.Handle(ctx, &node.ID, f.user); err != nil {
		t.Fatalf("first run: %v", err)
	}

	hasMeta, w, h, variants, err := f.store.MediaState(ctx, node.ContentHash)
	if err != nil || !hasMeta {
		t.Fatalf("MediaState: hasMeta=%v err=%v", hasMeta, err)
	}
	if len(variants) != 0 {
		t.Errorf("a 64x64 image rendered variants: %v", variants)
	}
	if got := media.ExpectedVariants("image/jpeg", w, h); len(got) != 0 {
		t.Errorf("ExpectedVariants(64x64) = %v, want none", got)
	}
}

// TestSharedPhotoKeepsItsThumbnailAndMetadata is the Phase 5 / Phase 7 seam.
//
// The download path was made grant-aware and the media reads were not, so a
// grantee could fetch a shared photo's full bytes and was told it had no media
// metadata and no thumbnail. serveMediaVariant's own comment explains what that
// costs: a grid of tiles quietly falls back to originals and "would pull
// gigabytes and look like a slow network instead of a missing job" — which is
// exactly what a shared album did.
func TestSharedPhotoKeepsItsThumbnailAndMetadata(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	other := f.other(t)

	folder := f.mkdir(f.root, "holiday")
	node, err := f.svc.Upload(ctx, f.user, folder.ID, "beach.jpg",
		bytes.NewReader(testJPEG(t, 1400, 900)), "image/jpeg")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	handler := media.NewHandler(
		files.NewMediaOpener(f.svc),
		files.NewMediaStore(f.store),
		files.NewMediaBlobWriter(f.svc),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := handler.Handle(ctx, &node.ID, f.user); err != nil {
		t.Fatalf("media job: %v", err)
	}

	// Shared as a folder, so the grant covers a photo it does not name.
	if _, err := f.store.CreateGrant(ctx, f.user, folder.ID, other, files.RoleViewer); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	meta, ok, err := f.store.MediaMetaForNode(ctx, other, node.ID)
	if err != nil || !ok {
		t.Fatalf("grantee got no media metadata: ok=%v err=%v", ok, err)
	}
	if meta.Width != 1400 || len(meta.Variants) == 0 {
		t.Errorf("grantee metadata = %dx%d variants=%v", meta.Width, meta.Height, meta.Variants)
	}

	metas, err := f.store.MediaMetaForNodes(ctx, other, []uuid.UUID{node.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := metas[node.ID]; !ok {
		t.Error("the batch read a shared album's grid uses skipped a shared photo")
	}

	if _, rc, err := f.svc.OpenMediaVariant(ctx, other, node.ID, media.VariantThumb); err != nil {
		t.Errorf("grantee could not read the shared photo's thumbnail: %v", err)
	} else {
		rc.Close()
	}

	// A stranger still gets nothing, which is what makes the above a share rather
	// than a hole.
	stranger := f.third(t)
	if _, ok, _ := f.store.MediaMetaForNode(ctx, stranger, node.ID); ok {
		t.Error("a stranger read this photo's metadata")
	}
	if _, _, err := f.svc.OpenMediaVariant(ctx, stranger, node.ID, media.VariantThumb); err == nil {
		t.Error("a stranger read this photo's thumbnail")
	}
}

// A variant belongs to whoever owns the node, not to whoever knows the node id.
// Content addressing means two accounts can share the underlying bytes, so the
// node row is the only thing that makes a thumbnail one caller's to read.
func TestMediaVariantIsScopedToTheOwner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	node, err := f.svc.Upload(ctx, f.user, f.root, "private.jpg",
		bytes.NewReader(testJPEG(t, 800, 600)), "image/jpeg")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	handler := media.NewHandler(
		files.NewMediaOpener(f.svc),
		files.NewMediaStore(f.store),
		files.NewMediaBlobWriter(f.svc),
		log,
	)
	if err := handler.Handle(ctx, &node.ID, f.user); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if _, _, err := f.svc.OpenMediaVariant(ctx, f.other(t), node.ID, media.VariantThumb); err == nil {
		t.Fatal("another user read this file's thumbnail")
	}
}

// testJPEG builds a decodable image of the requested size. A gradient rather
// than flat colour so the encoder produces realistic, non-degenerate output.
func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 0x40, 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}
