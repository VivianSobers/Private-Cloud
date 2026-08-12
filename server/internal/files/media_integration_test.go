package files_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

func TestMediaMetaRoundTrip(t *testing.T) {
	f := newFixture(t)

	node, err := f.svc.Upload(f.ctx, f.user, f.root, "photo.jpg",
		strings.NewReader("pretend jpeg "+uuid.NewString()), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}

	taken := time.Date(2019, 7, 14, 18, 22, 5, 0, time.UTC)
	lat, lon := 51.5072, -0.1276
	in := files.MediaMeta{
		Width: 4032, Height: 3024, Orientation: 6,
		TakenAt: &taken, Camera: "Pixel 8 Pro",
		GPSLat: &lat, GPSLon: &lon, Source: "image",
	}
	if err := f.store.PutMediaMeta(f.ctx, node.ContentHash, in); err != nil {
		t.Fatalf("PutMediaMeta: %v", err)
	}

	got, ok, err := f.store.MediaMetaForNode(f.ctx, f.user, node.ID)
	if err != nil || !ok {
		t.Fatalf("MediaMetaForNode: ok=%v err=%v", ok, err)
	}
	if got.Width != 4032 || got.Height != 3024 || got.Orientation != 6 {
		t.Errorf("dimensions/orientation = %dx%d o=%d", got.Width, got.Height, got.Orientation)
	}
	if got.TakenAt == nil || !got.TakenAt.Equal(taken) {
		t.Errorf("taken_at = %v, want %v", got.TakenAt, taken)
	}
	if got.Camera != "Pixel 8 Pro" {
		t.Errorf("camera = %q", got.Camera)
	}
	if got.GPSLat == nil || *got.GPSLat != lat {
		t.Errorf("gps lat = %v", got.GPSLat)
	}
	// No variants rendered yet, and that must be an empty list rather than null —
	// a client checks membership without a nil guard.
	if got.Variants == nil || len(got.Variants) != 0 {
		t.Errorf("variants = %v, want empty non-nil", got.Variants)
	}

	// Idempotent by content: writing again replaces rather than duplicating.
	if err := f.store.PutMediaMeta(f.ctx, node.ContentHash, in); err != nil {
		t.Fatalf("second PutMediaMeta: %v", err)
	}
	hasMeta, w, h, variants, err := f.store.MediaState(f.ctx, node.ContentHash)
	if err != nil || !hasMeta {
		t.Errorf("MediaState: hasMeta=%v err=%v", hasMeta, err)
	}
	if w != 4032 || h != 3024 {
		t.Errorf("MediaState dimensions = %dx%d, want 4032x3024", w, h)
	}
	if len(variants) != 0 {
		t.Errorf("MediaState variants = %v, want none", variants)
	}
}

// A file with no EXIF stores dimensions and nothing else — the common case, and
// the one where NULL handling is easiest to get wrong.
func TestMediaMetaWithoutEXIF(t *testing.T) {
	f := newFixture(t)
	node, err := f.svc.Upload(f.ctx, f.user, f.root, "screenshot.png",
		strings.NewReader("pretend png "+uuid.NewString()), "image/png")
	if err != nil {
		t.Fatal(err)
	}

	if err := f.store.PutMediaMeta(f.ctx, node.ContentHash, files.MediaMeta{
		Width: 800, Height: 600, Orientation: 1, Source: "image",
	}); err != nil {
		t.Fatalf("PutMediaMeta: %v", err)
	}
	got, ok, err := f.store.MediaMetaFor(f.ctx, node.ContentHash)
	if err != nil || !ok {
		t.Fatalf("MediaMetaFor: ok=%v err=%v", ok, err)
	}
	if got.TakenAt != nil || got.GPSLat != nil || got.GPSLon != nil || got.DurationMS != nil {
		t.Errorf("absent fields should stay nil: %+v", got)
	}
	if got.Camera != "" {
		t.Errorf("camera = %q, want empty", got.Camera)
	}
}

func TestMediaVariantRoundTripAndOwnership(t *testing.T) {
	f := newFixture(t)
	node, err := f.svc.Upload(f.ctx, f.user, f.root, "pic.jpg",
		strings.NewReader("bytes "+uuid.NewString()), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutMediaMeta(f.ctx, node.ContentHash, files.MediaMeta{
		Width: 4000, Height: 3000, Orientation: 1, Source: "image",
	}); err != nil {
		t.Fatal(err)
	}

	v := files.MediaVariant{
		Variant: "thumb", StorageKey: "va/" + uuid.NewString(),
		MIME: "image/jpeg", Size: 4096, Width: 320, Height: 240,
	}
	replaced, err := f.store.PutMediaVariant(f.ctx, node.ContentHash, v)
	if err != nil {
		t.Fatalf("PutMediaVariant: %v", err)
	}
	if replaced != "" {
		t.Errorf("first write displaced %q, want nothing", replaced)
	}

	got, err := f.store.MediaVariantFor(f.ctx, f.user, node.ID, "thumb")
	if err != nil {
		t.Fatalf("MediaVariantFor: %v", err)
	}
	if got.StorageKey != v.StorageKey || got.Width != 320 || got.Size != 4096 {
		t.Errorf("variant round-trip mismatch: %+v", got)
	}

	// It now shows up in the metadata's variant list, which is how a gallery
	// decides whether to ask for a thumbnail at all.
	meta, _, err := f.store.MediaMetaFor(f.ctx, node.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Variants) != 1 || meta.Variants[0] != "thumb" {
		t.Errorf("variants = %v, want [thumb]", meta.Variants)
	}

	// Re-rendering to a NEW key reports the old one so the caller can delete it.
	v2 := v
	v2.StorageKey = "va/" + uuid.NewString()
	replaced, err = f.store.PutMediaVariant(f.ctx, node.ContentHash, v2)
	if err != nil {
		t.Fatal(err)
	}
	if replaced != v.StorageKey {
		t.Errorf("displaced key = %q, want %q", replaced, v.StorageKey)
	}
	// Re-writing the SAME key displaces nothing — deleting it would delete the
	// bytes that were just stored.
	replaced, err = f.store.PutMediaVariant(f.ctx, node.ContentHash, v2)
	if err != nil {
		t.Fatal(err)
	}
	if replaced != "" {
		t.Errorf("identical re-write displaced %q, want nothing", replaced)
	}

	// A different user must not reach it, even though the variant is keyed by
	// content and content is shared by dedup. This is the whole reason the
	// ownership join is in the lookup query rather than a check beside it.
	other := newFixture(t)
	if _, err := f.store.MediaVariantFor(f.ctx, other.user, node.ID, "thumb"); err == nil {
		t.Error("another user reached this file's thumbnail")
	}

	// A variant that was never rendered is a miss, not an error to guess at.
	if _, err := f.store.MediaVariantFor(f.ctx, f.user, node.ID, "preview"); err == nil {
		t.Error("missing variant should not be found")
	}
}

// Variants and metadata for content nothing references any more are reclaimable,
// and the reclaim must not touch content that is still live.
func TestMediaGCOnlyTakesUnreferencedContent(t *testing.T) {
	f := newFixture(t)
	live, err := f.svc.Upload(f.ctx, f.user, f.root, "keep.jpg",
		strings.NewReader("live "+uuid.NewString()), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutMediaMeta(f.ctx, live.ContentHash, files.MediaMeta{
		Width: 10, Height: 10, Orientation: 1, Source: "image",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PutMediaVariant(f.ctx, live.ContentHash, files.MediaVariant{
		Variant: "thumb", StorageKey: "va/live", MIME: "image/jpeg",
		Size: 1, Width: 1, Height: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Orphaned rows: a content hash no version has ever referenced.
	orphan := []byte(strings.Repeat("z", 32))
	if err := f.store.PutMediaMeta(f.ctx, orphan, files.MediaMeta{
		Width: 10, Height: 10, Orientation: 1, Source: "image",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PutMediaVariant(f.ctx, orphan, files.MediaVariant{
		Variant: "thumb", StorageKey: "va/orphan", MIME: "image/jpeg",
		Size: 1, Width: 1, Height: 1,
	}); err != nil {
		t.Fatal(err)
	}

	vs, hashes, err := f.store.UnreferencedMediaVariants(f.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var sawOrphan, sawLive bool
	for i, v := range vs {
		if v.StorageKey == "va/orphan" && string(hashes[i]) == string(orphan) {
			sawOrphan = true
		}
		if v.StorageKey == "va/live" {
			sawLive = true
		}
	}
	if !sawOrphan {
		t.Error("orphaned variant not listed for collection")
	}
	if sawLive {
		t.Fatal("a live file's variant was listed for collection")
	}

	if deleted, err := f.store.DeleteMediaVariantRow(f.ctx, orphan, "thumb"); err != nil || !deleted {
		t.Errorf("DeleteMediaVariantRow: %v %v", deleted, err)
	}
	// The live one must refuse deletion even if asked directly — the re-check
	// inside the delete is what makes losing the list/delete race harmless.
	if deleted, err := f.store.DeleteMediaVariantRow(f.ctx, live.ContentHash, "thumb"); err != nil || deleted {
		t.Errorf("live variant was deletable: %v %v", deleted, err)
	}

	if _, err := f.store.PruneMediaMeta(f.ctx, 100); err != nil {
		t.Fatal(err)
	}
	if has, _, _, _, _ := f.store.MediaState(f.ctx, orphan); has {
		t.Error("orphaned media_meta survived the prune")
	}
	if has, _, _, _, _ := f.store.MediaState(f.ctx, live.ContentHash); !has {
		t.Error("live media_meta was pruned")
	}

	// fsck needs every variant key, or it calls them orphans and --repair
	// deletes every thumbnail in the system.
	keys, err := f.store.MediaVariantKeys(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["va/live"]; !ok {
		t.Error("live variant key missing from the fsck set")
	}
}
