package files_test

// Slice 1b: the upload path routes through the chunker.
//
// These tests pin the routing decision (threshold), the invariants that make
// dedup safe to expose to users (quota counts logical bytes, always), and the
// full lifecycle of a manifest-backed file: upload, read back, range-seek,
// overwrite, trash, purge, GC to zero.

import (
	"bytes"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// uniqueData returns content that cannot collide with earlier test runs: the
// test database persists between runs, and manifest reuse would otherwise make
// "first upload" assertions fail for reasons unrelated to the code under test.
func uniqueData(n int, seed int64) []byte {
	return append([]byte(uuid.NewString()), casData(n, seed)...)
}

func (f *fixture) uploadBytes(name string, data []byte) *files.Node {
	f.t.Helper()
	n, err := f.svc.Upload(f.ctx, f.user, f.root, name, bytes.NewReader(data), "")
	if err != nil {
		f.t.Fatalf("Upload(%q): %v", name, err)
	}
	return n
}

func (f *fixture) readBack(id uuid.UUID) []byte {
	f.t.Helper()
	_, rc, err := f.svc.Open(f.ctx, f.user, id)
	if err != nil {
		f.t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		f.t.Fatalf("read: %v", err)
	}
	return got
}

func TestUploadRoutesThroughCAS(t *testing.T) {
	f, _ := casFixture(t)

	data := uniqueData(300<<10, 40)
	node := f.uploadBytes("big.bin", data)

	if node.ManifestID == nil {
		t.Fatal("an upload above the threshold did not produce a manifest-backed version")
	}
	if node.BlobKey != "" {
		t.Error("a manifest-backed version also carries a blob key; the schema says exactly one")
	}
	if node.Size != int64(len(data)) {
		t.Errorf("node size = %d, want %d", node.Size, len(data))
	}
	if len(node.ContentHash) != 32 {
		t.Errorf("content hash is %d bytes, want 32 (BLAKE3-256)", len(node.ContentHash))
	}

	// The version row itself must be manifest-only. (Asserted in SQL rather
	// than via AllBlobKeys, which reads the whole shared test database and
	// would count every other fixture's blobs.)
	var blobID *uuid.UUID
	var manifestID *uuid.UUID
	if err := f.store.Pool().QueryRow(f.ctx, `
		SELECT blob_id, manifest_id FROM file_versions WHERE id = $1`,
		node.HeadVersionID).Scan(&blobID, &manifestID); err != nil {
		t.Fatal(err)
	}
	if blobID != nil {
		t.Error("a chunked upload recorded a blob_id on its version")
	}
	if manifestID == nil {
		t.Error("a chunked upload recorded no manifest_id on its version")
	}

	if got := f.readBack(node.ID); !bytes.Equal(got, data) {
		t.Error("chunked upload did not round-trip byte-identically")
	}
}

func TestSmallUploadStaysWholeFile(t *testing.T) {
	// Below cas.WholeFileThreshold a manifest row plus a chunk row is pure
	// overhead; small files stay whole-file blobs permanently.
	f, _ := casFixture(t)

	data := uniqueData(200, 41) // uuid prefix + 200 bytes, still well under 2 KiB
	node := f.uploadBytes("small.txt", data)

	if node.ManifestID != nil {
		t.Fatal("a sub-threshold upload was chunked; small files must stay whole blobs")
	}
	if node.BlobKey == "" {
		t.Fatal("a blob-backed version has no storage key")
	}
	if got := f.readBack(node.ID); !bytes.Equal(got, data) {
		t.Error("small upload did not round-trip byte-identically")
	}
}

func TestUploadExactlyAtThresholdIsChunked(t *testing.T) {
	// The boundary itself: >= threshold chunks. Pinned so an off-by-one in the
	// peek router shows up here rather than as a puzzling format split in
	// production data.
	f, _ := casFixture(t)

	node := f.uploadBytes("edge.bin", casData(cas.WholeFileThreshold, 42))
	if node.ManifestID == nil {
		t.Error("an upload of exactly the threshold size was not chunked")
	}
}

func TestIdenticalUploadReusesManifest(t *testing.T) {
	// Whole-file dedup: same content, second file, no new storage rows at all.
	// Reuse requires the first manifest to be referenced by a live version,
	// which is exactly what the first upload provides.
	f, _ := casFixture(t)

	data := uniqueData(128<<10, 43)
	first := f.uploadBytes("one.bin", data)
	second := f.uploadBytes("two.bin", data)

	if first.ManifestID == nil || second.ManifestID == nil {
		t.Fatal("uploads above the threshold were not chunked")
	}
	if *first.ManifestID != *second.ManifestID {
		t.Error("identical content produced two manifests; the second upload should reuse the first")
	}

	// Both files must stay independently readable — and readable after one of
	// them is purged, because the manifest is shared.
	if _, err := f.store.Trash(f.ctx, f.user, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Purge(f.ctx, f.user, first.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.BlobGCGrace = 0
	if _, err := f.svc.CollectGarbage(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.readBack(second.ID); !bytes.Equal(got, data) {
		t.Error("purging one file of a deduplicated pair broke the survivor")
	}
}

func TestQuotaCountsLogicalBytesUnderDedup(t *testing.T) {
	// Dedup means the second identical upload costs ~zero physical bytes. The
	// quota must charge it in full anyway: billing by physical share would be
	// unpredictable and would leak the existence of other content. This is the
	// §4 invariant from the design doc, as a test.
	f, _ := casFixture(t)

	data := uniqueData(64<<10, 44)
	size := int64(len(data))

	quota := size + size/2 // fits one copy, not two
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE users SET quota_bytes = $2 WHERE id = $1`, f.user, quota); err != nil {
		t.Fatal(err)
	}

	first := f.uploadBytes("first.bin", data)

	_, err := f.svc.Upload(f.ctx, f.user, f.root, "second.bin", bytes.NewReader(data), "")
	if err == nil {
		t.Fatal("a deduplicated upload was admitted past the quota — quota must count logical bytes")
	}

	// The rejected upload reused the first file's manifest, so its failure
	// cleanup must NOT have touched it. This is the ReusedManifest guard: an
	// unguarded delete here would sever a healthy file's content.
	if got := f.readBack(first.ID); !bytes.Equal(got, data) {
		t.Error("a rejected duplicate upload broke the file it deduplicated against")
	}
}

func TestFinishStagedRoutesThroughCAS(t *testing.T) {
	// The resumable path: bytes arrive in pieces into staging, and the finish
	// decides the storage format. Above the threshold that must be a manifest.
	f, _ := casFixture(t)

	data := uniqueData(96<<10, 45)
	sess, err := f.svc.CreateUpload(f.ctx, f.user, f.root, "resumable.bin", int64(len(data)), "")
	if err != nil {
		t.Fatal(err)
	}

	// Two appends, because resuming mid-file is the feature.
	half := len(data) / 2
	if _, err := f.svc.AppendChunk(f.ctx, f.user, sess.ID, 0, bytes.NewReader(data[:half])); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AppendChunk(f.ctx, f.user, sess.ID, int64(half), bytes.NewReader(data[half:])); err != nil {
		t.Fatal(err)
	}

	node, err := f.svc.FinishUpload(f.ctx, f.user, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if node.ManifestID == nil {
		t.Fatal("a finished resumable upload above the threshold is not manifest-backed")
	}
	if got := f.readBack(node.ID); !bytes.Equal(got, data) {
		t.Error("resumable chunked upload did not round-trip byte-identically")
	}

	// The staging file must be gone: chunking reads it in place, so nothing
	// renames it away and an unswept leftover would sit there until GC.
	stager := f.svc.Blobs().(interface {
		WalkStaging(fn func(key string, size int64) error) error
	})
	var staged int
	if err := stager.WalkStaging(func(string, int64) error { staged++; return nil }); err != nil {
		t.Fatal(err)
	}
	if staged != 0 {
		t.Errorf("%d staging file(s) left behind after a chunked finish", staged)
	}
}

func TestFinishStagedSmallFileStaysWholeFile(t *testing.T) {
	f, _ := casFixture(t)

	data := []byte("tiny resumable payload")
	sess, err := f.svc.CreateUpload(f.ctx, f.user, f.root, "tiny.txt", int64(len(data)), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AppendChunk(f.ctx, f.user, sess.ID, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	node, err := f.svc.FinishUpload(f.ctx, f.user, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if node.ManifestID != nil {
		t.Error("a sub-threshold resumable upload was chunked")
	}
	if got := f.readBack(node.ID); !bytes.Equal(got, data) {
		t.Error("small resumable upload did not round-trip")
	}
}

func TestOverwriteBlobFileWithChunkedVersion(t *testing.T) {
	// Both formats coexist on one NODE across versions: a Phase 1 file
	// overwritten after slice 1b gets a manifest-backed head while the old
	// version keeps its blob. The reader follows the head.
	f, _ := casFixture(t)

	small := uniqueData(100, 46)
	node := f.uploadBytes("grows.bin", small)
	if node.ManifestID != nil {
		t.Fatal("setup: first version should be blob-backed")
	}

	big := uniqueData(200<<10, 47)
	node2 := f.uploadBytes("grows.bin", big)
	if node2.ID != node.ID {
		t.Fatal("overwriting by name created a second node")
	}
	if node2.ManifestID == nil {
		t.Fatal("the overwriting version should be manifest-backed")
	}
	if got := f.readBack(node.ID); !bytes.Equal(got, big) {
		t.Error("head version after overwrite does not serve the new content")
	}
}

func TestFailedPutFileDropsOrphanManifest(t *testing.T) {
	// When recording the file fails after the chunks are durable, the manifest
	// must not linger until GC: the service deletes it immediately (row only —
	// chunk bytes may already be shared and belong to the GC's refcount path).
	f, store := casFixture(t)

	// A folder occupying the target name makes PutFile fail with ErrNameTaken.
	f.mkdir(f.root, "occupied")

	data := uniqueData(64<<10, 48)
	_, err := f.svc.Upload(f.ctx, f.user, f.root, "occupied", bytes.NewReader(data), "")
	if err == nil {
		t.Fatal("uploading over a folder succeeded")
	}

	var manifests int
	if err := f.store.Pool().QueryRow(f.ctx, `
		SELECT count(*) FROM manifests m
		WHERE NOT EXISTS (SELECT 1 FROM file_versions v WHERE v.manifest_id = m.id)
		  AND m.created_at > now() - interval '1 minute'`).Scan(&manifests); err != nil {
		t.Fatal(err)
	}
	if manifests != 0 {
		t.Errorf("%d orphan manifest(s) left after a failed upload; expected immediate cleanup", manifests)
	}

	// And a GC pass reclaims the now-unreferenced chunks entirely.
	f.svc.BlobGCGrace = 0
	if _, err := f.svc.CollectGarbage(f.ctx); err != nil {
		t.Fatal(err)
	}
	report, err := f.svc.Fsck(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.ChunksChecked != 0 {
		t.Errorf("%d chunk(s) survived GC after their only manifest was dropped", report.ChunksChecked)
	}
	_ = store
}

func TestServiceOpenSeeksAcrossChunks(t *testing.T) {
	// The service-level reader must honour Range-style access for chunked
	// files, because http.ServeContent is what calls it.
	f, _ := casFixture(t)

	data := uniqueData(300<<10, 49)
	node := f.uploadBytes("video.bin", data)
	if node.ManifestID == nil {
		t.Fatal("setup: upload was not chunked")
	}

	_, rc, err := f.svc.Open(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	off := int64(len(data)) - 1024
	if _, err := rc.Seek(off, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	tail := make([]byte, 1024)
	if _, err := io.ReadFull(rc, tail); err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if !bytes.Equal(tail, data[off:]) {
		t.Error("seek+read across chunk boundaries returned wrong bytes")
	}
}
