package files_test

import (
	"bytes"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// These tests exist because of a real defect, not a hypothetical one.
//
// fsck walks the blob store and reports anything absent from the `blobs` table
// as an orphan. Chunks live in that SAME root under the SAME ab/cd/hash layout
// but are recorded in `chunks` — so before this was fixed, `fsck --repair`
// would have deleted every deduplicated byte in the system the moment CAS went
// live. Nothing caught it because nothing wrote chunks yet.

func casFixture(t *testing.T) (*fixture, *cas.Store) {
	t.Helper()
	f := newFixture(t)

	store, err := cas.NewStore(f.store.Pool(), f.svc.Blobs())
	if err != nil {
		t.Fatalf("cas.NewStore: %v", err)
	}
	f.svc.SetCAS(store)
	return f, store
}

func casData(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestFsckDoesNotTreatChunksAsOrphans(t *testing.T) {
	f, store := casFixture(t)

	if _, err := store.Write(f.ctx, bytes.NewReader(casData(512<<10, 1))); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// What this fixture actually put on disk.
	onDisk := map[string]bool{}
	fs := f.svc.Blobs().(*blob.FSStore)
	if err := fs.Walk(func(key string, _ int64) error {
		onDisk[key] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(onDisk) == 0 {
		t.Fatal("the write produced no chunks on disk")
	}

	report, err := f.svc.Fsck(f.ctx, false)
	if err != nil {
		t.Fatalf("Fsck: %v", err)
	}

	// The bug being guarded: chunks on disk must never be classified as
	// orphans, because `--repair` deletes orphans.
	if len(report.Orphans) != 0 {
		t.Fatalf("fsck reported %d chunk(s) as orphans — `--repair` would delete "+
			"every deduplicated byte in the system", len(report.Orphans))
	}
	if report.ChunksChecked != len(onDisk) {
		t.Errorf("fsck accounted for %d chunks, %d are on disk", report.ChunksChecked, len(onDisk))
	}

	// MissingChunks is asserted by membership. fsck compares the whole chunks
	// table against ONE blob store — correct in production, where there is
	// exactly one — but here every other fixture has its own temp directory and
	// its rows are legitimately absent from this one.
	for _, key := range report.MissingChunks {
		if onDisk[key] {
			t.Errorf("fsck reported chunk %q missing while it is on disk", key)
		}
	}
	if len(report.RefcountDrift) != 0 {
		t.Errorf("refcount drift straight after a write: %v", report.RefcountDrift)
	}
}

func TestFsckRepairLeavesChunksAlone(t *testing.T) {
	// The destructive half of the same bug: proving --repair is safe matters
	// more than proving the report is right.
	f, store := casFixture(t)

	data := casData(256<<10, 2)
	res, err := store.Write(f.ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Fsck(f.ctx, true); err != nil {
		t.Fatalf("Fsck(repair): %v", err)
	}

	// The content must still be readable, byte for byte, after a repair pass.
	rc, err := store.Open(f.ctx, res.Manifest.ID)
	if err != nil {
		t.Fatalf("Open after repair: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read after repair: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("content changed across an fsck --repair")
	}
}

func TestFsckReportsMissingChunks(t *testing.T) {
	f, store := casFixture(t)

	res, err := store.Write(f.ctx, bytes.NewReader(casData(64<<10, 3)))
	if err != nil {
		t.Fatal(err)
	}

	// Delete one chunk's bytes behind the database's back.
	fs := f.svc.Blobs().(*blob.FSStore)
	var victim string
	_ = fs.Walk(func(key string, _ int64) error {
		if victim == "" {
			victim = key
		}
		return nil
	})
	if victim == "" {
		t.Fatal("no chunk on disk to remove")
	}
	if err := fs.Delete(f.ctx, victim); err != nil {
		t.Fatal(err)
	}

	report, err := f.svc.Fsck(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(report.MissingChunks, victim) {
		t.Errorf("deleted chunk %q was not reported missing: %v", victim, report.MissingChunks)
	}

	// And reading through it must fail loudly rather than serve a hole.
	rc, err := store.Open(f.ctx, res.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil {
		t.Error("reading a file with a missing chunk succeeded; it must fail loudly")
	}
}

func TestChunkGCReclaimsUnreferencedChunks(t *testing.T) {
	f, store := casFixture(t)

	// A manifest is "live" only while a file version references it. Since
	// slice 1b the GC reaps orphaned manifests itself, so the referenced state
	// has to be real here, not just "the manifest row exists".
	res, err := store.Write(f.ctx, bytes.NewReader(casData(128<<10, 4)))
	if err != nil {
		t.Fatal(err)
	}
	node, err := f.store.PutFile(f.ctx, files.PutFileInput{
		OwnerID: f.user, ParentID: f.root, Name: "gc-victim.bin",
		ManifestID: res.Manifest.ID, Size: res.LogicalSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.BlobGCGrace = 0

	// Referenced by a live version: GC must not touch manifest or chunks.
	if _, err := f.svc.CollectGarbage(f.ctx); err != nil {
		t.Fatal(err)
	}
	rc, err := store.Open(f.ctx, res.Manifest.ID)
	if err != nil {
		t.Fatalf("GC deleted chunks of a live manifest: %v", err)
	}
	rc.Close()

	// Trash and purge the file. The version rows cascade away, orphaning the
	// manifest; the GC's manifest pass must then walk the whole chain down:
	// manifest row -> manifest_chunks cascade -> refcount trigger -> chunk
	// rows -> chunk bytes.
	if _, err := f.store.Trash(f.ctx, f.user, node.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Purge(f.ctx, f.user, node.ID); err != nil {
		t.Fatal(err)
	}

	gc, err := f.svc.CollectGarbage(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gc.ManifestsFreed == 0 {
		t.Error("GC freed no manifests after the referencing version was purged")
	}
	if gc.ChunksFreed == 0 {
		t.Error("GC freed no chunks after the manifest was deleted — " +
			"the refcount trigger did not fire on cascade")
	}

	// And nothing of it survives on disk.
	report, err := f.svc.Fsck(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.ChunksChecked != 0 {
		t.Errorf("%d chunk(s) still on disk after GC", report.ChunksChecked)
	}
}

func TestDedupAcrossUsers(t *testing.T) {
	// The whole point of CAS. Two people uploading the same content must
	// converge on one set of chunks without either knowing the other exists.
	f, store := casFixture(t)

	// Unique per run. The test database persists between runs, so fixed content
	// would already be stored and the "first" write would dedup against a
	// previous run — making the assertion below fail for a reason that has
	// nothing to do with the behaviour under test.
	data := append([]byte(uuid.NewString()), casData(256<<10, 5)...)

	first, err := store.Write(f.ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if first.DedupedChunks != 0 {
		t.Errorf("first write deduped %d chunks against content never stored before",
			first.DedupedChunks)
	}

	second, err := store.Write(f.ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if second.NewChunks != 0 {
		t.Errorf("re-uploading identical content wrote %d new chunks, want 0", second.NewChunks)
	}
	if second.NewBytes != 0 {
		t.Errorf("re-uploading identical content wrote %d new bytes, want 0", second.NewBytes)
	}

	// Distinct manifests, shared chunks — each file keeps its own identity.
	if first.Manifest.ID == second.Manifest.ID {
		t.Error("the second upload reused the first manifest row")
	}
	if first.Manifest.ContentHash != second.Manifest.ContentHash {
		t.Error("identical content produced different whole-file hashes")
	}
}

func TestReassembledContentSurvivesRoundTrip(t *testing.T) {
	f, store := casFixture(t)

	for _, size := range []int{1, 100, MinChunk - 1, 64 << 10, 1 << 20, (2 << 20) + 7} {
		data := casData(size, int64(size))
		res, err := store.Write(f.ctx, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Write(%d bytes): %v", size, err)
		}
		if res.Manifest.TotalSize != int64(size) {
			t.Errorf("manifest total_size = %d, want %d", res.Manifest.TotalSize, size)
		}

		rc, err := store.Open(f.ctx, res.Manifest.ID)
		if err != nil {
			t.Fatalf("Open(%d bytes): %v", size, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read(%d bytes): %v", size, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("%d-byte round trip produced different content", size)
		}
	}
}

// MinChunk mirrors cas.MinChunkSize without importing it into the loop above.
const MinChunk = 2 << 10

func TestSeekSupportsRangeRequests(t *testing.T) {
	// http.ServeContent seeks to serve a Range. Reassembly that could only
	// stream forward would make video scrubbing re-read from byte zero.
	f, store := casFixture(t)

	data := casData(512<<10, 6)
	res, err := store.Write(f.ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	rc, err := store.Open(f.ctx, res.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	for _, tc := range []struct{ off, n int64 }{
		{0, 100},
		{1000, 5000},
		{int64(len(data)) - 50, 50},
		{200 << 10, 1 << 10},
	} {
		if _, err := rc.Seek(tc.off, io.SeekStart); err != nil {
			t.Fatalf("Seek(%d): %v", tc.off, err)
		}
		got := make([]byte, tc.n)
		if _, err := io.ReadFull(rc, got); err != nil {
			t.Fatalf("ReadFull at %d: %v", tc.off, err)
		}
		if !bytes.Equal(got, data[tc.off:tc.off+tc.n]) {
			t.Errorf("bytes at offset %d do not match the source", tc.off)
		}
	}

	// Seeking backwards must work too — clients rewind.
	if _, err := rc.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 64)
	if _, err := io.ReadFull(rc, head); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(head, data[:64]) {
		t.Error("seeking backwards returned the wrong bytes")
	}

	// SeekEnd is how ServeContent discovers the length.
	end, err := rc.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if end != int64(len(data)) {
		t.Errorf("SeekEnd = %d, want %d", end, len(data))
	}
}

func TestReadDetectsCorruptedChunk(t *testing.T) {
	// Every chunk is verified against its address before being served. This is
	// the only thing between silent bit-rot and serving corruption as truth.
	f, store := casFixture(t)

	res, err := store.Write(f.ctx, strings.NewReader(strings.Repeat("verify me. ", 40000)))
	if err != nil {
		t.Fatal(err)
	}

	fs := f.svc.Blobs().(*blob.FSStore)
	var victim string
	_ = fs.Walk(func(key string, _ int64) error {
		if victim == "" {
			victim = key
		}
		return nil
	})
	// Replace the chunk's bytes with something of the same length that does not
	// hash to its key.
	if err := fs.Delete(f.ctx, victim); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.PutKeyed(f.ctx, victim, strings.NewReader("corrupted")); err != nil {
		t.Fatal(err)
	}

	rc, err := store.Open(f.ctx, res.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	if _, err := io.ReadAll(rc); err == nil {
		t.Error("a corrupted chunk was served without complaint")
	}
}

func TestStatsReportSavings(t *testing.T) {
	f, store := casFixture(t)

	before, err := store.Stats(f.ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Highly compressible, uploaded twice so dedup has something to show, and
	// seeded uniquely per run. The test database persists across runs, so fixed
	// content would already be stored and the second write would measure a saving
	// of zero.
	data := bytes.Repeat([]byte(uuid.NewString()+" the same line over and over\n"), 20000)
	first, err := store.Write(f.ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Write(f.ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	// The savings are asserted from the WRITE RESULTS, not from global Stats
	// deltas: Stats is server-wide, and once concurrent packages also write
	// manifests, a before/after subtraction is no longer this test's alone. The
	// per-call result reports exactly what each write cost.
	if first.NewChunks == 0 || first.NewBytes == 0 {
		t.Error("the first write of fresh content stored nothing")
	}
	// Compression: what actually hit disk is below the logical size for this
	// highly compressible content.
	if first.NewBytes >= int64(len(data)) {
		t.Errorf("stored %d bytes for %d logical — compression did not help", first.NewBytes, len(data))
	}
	// Dedup: the second, identical write added nothing at all.
	if second.NewChunks != 0 || second.NewBytes != 0 {
		t.Errorf("re-uploading identical content added %d chunk(s), %d byte(s), want 0",
			second.NewChunks, second.NewBytes)
	}
	if second.Manifest.ID == first.Manifest.ID {
		t.Error("the second upload reused the first manifest row rather than its own")
	}

	// Stats is still exercised, but only for the property it can guarantee under
	// concurrency: it moved forward and includes this test's two manifests.
	after, err := store.Stats(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Manifests-before.Manifests < 2 {
		t.Errorf("Stats manifests grew by %d, want at least this test's 2", after.Manifests-before.Manifests)
	}
	if after.LogicalBytes-before.LogicalBytes < int64(2*len(data)) {
		t.Errorf("Stats logical bytes grew by %d, want at least %d", after.LogicalBytes-before.LogicalBytes, 2*len(data))
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
