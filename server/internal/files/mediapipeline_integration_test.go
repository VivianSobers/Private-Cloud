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
