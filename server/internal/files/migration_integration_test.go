package files_test

// Slice 2: background migration of Phase 1 whole-file blobs into chunks.
//
// The scenario every test here reconstructs is the real one: content was
// uploaded before CAS existed (a blob-backed version), CAS is switched on, and
// the drain must rewrite that content into chunks WITHOUT the file it represents
// changing by a single byte, without touching quota, and without leaking the old
// blob. Both formats coexist, so a reader must not be able to tell which path a
// file took.
//
// MigrateBlobs operates on the WHOLE database — that is the production contract,
// not a test convenience — and the integration database is shared across
// fixtures and across concurrently running package binaries. So these tests
// scope every assertion to their OWN nodes and never to a global return count:
// another fixture's blob (whose bytes live on a different temp dir) legitimately
// shows up as a Failed candidate here, and the oldest-first batch would exclude
// this test's newest file. migrateAllLimit sidesteps the second problem; owner-
// scoped assertions sidestep the first.

import (
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
)

// migrateAllLimit is large enough that a single pass processes every candidate
// in the shared database, so a test's own file is always reached no matter how
// many unrelated blob-backed versions other fixtures have left lying around.
const migrateAllLimit = 1 << 20

// enableCAS turns on content-addressing AFTER blob-backed files already exist,
// which is the whole point — casFixture attaches it before anything is written,
// and migration is about content that predates it.
func (f *fixture) enableCAS(t *testing.T) *cas.Store {
	t.Helper()
	store, err := cas.NewStore(f.store.Pool(), f.svc.Blobs())
	if err != nil {
		t.Fatalf("cas.NewStore: %v", err)
	}
	f.svc.SetCAS(store)
	return store
}

// blobRefcount reads a blob row's reference count straight from the table, so a
// test can prove the trigger fired on an in-place format switch.
func (f *fixture) blobRefcount(t *testing.T, storageKey string) int64 {
	t.Helper()
	var n int64
	err := f.store.Pool().QueryRow(f.ctx,
		`SELECT refcount FROM blobs WHERE storage_key = $1`, storageKey).Scan(&n)
	if err != nil {
		t.Fatalf("read blob refcount: %v", err)
	}
	return n
}

// manifestID returns a live node's manifest id, or nil if it is still
// blob-backed — the single fact almost every migration assertion turns on.
func (f *fixture) manifestID(t *testing.T, id uuid.UUID) *uuid.UUID {
	t.Helper()
	n, err := f.store.Get(f.ctx, f.user, id)
	if err != nil {
		t.Fatal(err)
	}
	return n.ManifestID
}

func TestMigrateRepointsToManifest(t *testing.T) {
	f := newFixture(t)

	// Uploaded with no CAS attached: a genuine Phase 1 whole-file blob.
	data := uniqueData(64<<10, 1)
	node := f.uploadBytes("report.bin", data)
	if node.ManifestID != nil {
		t.Fatal("file was chunked before CAS was enabled")
	}
	if node.BlobKey == "" {
		t.Fatal("blob-backed file has no storage key")
	}

	f.enableCAS(t)
	res, err := f.svc.MigrateBlobs(f.ctx, migrateAllLimit)
	if err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}
	if res.VersionsMigrated < 1 {
		t.Fatalf("migrated %d version(s), want at least this file's one", res.VersionsMigrated)
	}

	// The node now reads as content-addressed, and the bytes survive the switch.
	got, err := f.store.Get(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ManifestID == nil {
		t.Error("version still blob-backed after migration")
	}
	if got.BlobKey != "" {
		t.Error("migrated version still carries a blob key")
	}
	if out := f.readBack(node.ID); string(out) != string(data) {
		t.Error("content changed across migration")
	}
}

func TestMigratePreservesContentAcrossSizes(t *testing.T) {
	// A single chunk, a boundary case, and a many-chunk file: reassembly after
	// migration must be byte-exact at every size, because a mismatch is silent
	// corruption of a file the user never touched.
	f := newFixture(t)

	type item struct {
		id   uuid.UUID
		data []byte
	}
	var items []item
	for i, size := range []int{2 << 10, 64 << 10, (1 << 20) + 7} {
		data := uniqueData(size, int64(100+i))
		n := f.uploadBytes(uuid.NewString()+".bin", data)
		items = append(items, item{n.ID, data})
	}

	f.enableCAS(t)
	if _, err := f.svc.MigrateBlobs(f.ctx, migrateAllLimit); err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}

	for _, it := range items {
		if f.manifestID(t, it.id) == nil {
			t.Errorf("%d-byte file was not migrated", len(it.data))
		}
		if got := f.readBack(it.id); string(got) != string(it.data) {
			t.Errorf("%d-byte file changed across migration", len(it.data))
		}
	}
}

func TestMigrateDropsOldBlobToZeroThenGC(t *testing.T) {
	// The reason migration 00008 exists. The in-place UPDATE moves the reference
	// off the blob; the AFTER UPDATE trigger must decrement it to zero, or GC —
	// which only reclaims blobs at zero — would leak the bytes forever, in the
	// phase whose entire point is to store less.
	f := newFixture(t)

	node := f.uploadBytes("leaky.bin", uniqueData(48<<10, 2))
	key := node.BlobKey
	if rc := f.blobRefcount(t, key); rc != 1 {
		t.Fatalf("blob refcount before migration = %d, want 1", rc)
	}

	f.enableCAS(t)
	if _, err := f.svc.MigrateBlobs(f.ctx, migrateAllLimit); err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}
	if rc := f.blobRefcount(t, key); rc != 0 {
		t.Fatalf("blob refcount after migration = %d, want 0 — the UPDATE trigger did not fire", rc)
	}

	// With the reference gone, GC must reclaim this row and its bytes.
	f.svc.BlobGCGrace = 0
	if _, err := f.svc.CollectGarbage(f.ctx); err != nil {
		t.Fatal(err)
	}

	var exists bool
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT EXISTS(SELECT 1 FROM blobs WHERE storage_key = $1)`, key).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("old blob row survived GC after migration")
	}
}

func TestMigrateSkipsSmallFiles(t *testing.T) {
	// Below the chunker threshold a file stays a whole-file blob permanently: a
	// manifest row plus a chunk row to describe a few hundred bytes is pure
	// overhead. Migration must leave those exactly as they are.
	f := newFixture(t)

	node := f.uploadBytes("tiny.txt", uniqueData(200, 3))
	if node.BlobKey == "" {
		t.Fatal("small file was not stored as a blob")
	}

	f.enableCAS(t)
	if _, err := f.svc.MigrateBlobs(f.ctx, migrateAllLimit); err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}

	if f.manifestID(t, node.ID) != nil {
		t.Error("a sub-threshold file was chunked by migration")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	// A second pass must not touch an already-chunked version: chunked versions
	// are not candidates, so re-running the drain — or a background loop
	// overlapping a manual run — leaves the file on the exact manifest it already
	// had, never rewriting it.
	f := newFixture(t)
	node := f.uploadBytes("once.bin", uniqueData(32<<10, 4))
	f.enableCAS(t)

	if _, err := f.svc.MigrateBlobs(f.ctx, migrateAllLimit); err != nil {
		t.Fatal(err)
	}
	first := f.manifestID(t, node.ID)
	if first == nil {
		t.Fatal("file was not migrated on the first pass")
	}

	if _, err := f.svc.MigrateBlobs(f.ctx, migrateAllLimit); err != nil {
		t.Fatal(err)
	}
	second := f.manifestID(t, node.ID)
	if second == nil || *second != *first {
		t.Errorf("a second pass moved the file off its manifest: %v -> %v", first, second)
	}
}

func TestMigratePreservesQuota(t *testing.T) {
	// Quota counts logical bytes — the file as its owner understands it — never
	// physical chunks. Migration changes only the physical representation, so the
	// number a user sees must not move even a byte when their file is chunked.
	f := newFixture(t)
	f.uploadBytes("q.bin", uniqueData(80<<10, 5))

	before, err := f.store.Usage(f.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}

	f.enableCAS(t)
	if _, err := f.svc.MigrateBlobs(f.ctx, migrateAllLimit); err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}

	after, err := f.store.Usage(f.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	if after.LiveBytes != before.LiveBytes {
		t.Errorf("live bytes moved across migration: %d -> %d", before.LiveBytes, after.LiveBytes)
	}
}

func TestMigrateDedupsIdenticalBlobs(t *testing.T) {
	// Two Phase 1 blobs holding identical content should converge on ONE manifest
	// as they migrate — the first builds it, the second reuses it. This is dedup
	// arriving retroactively for content stored before CAS existed.
	f := newFixture(t)
	data := uniqueData(96<<10, 6)
	a := f.uploadBytes("a.bin", data)
	b := f.uploadBytes("b.bin", data)

	f.enableCAS(t)
	res, err := f.svc.MigrateBlobs(f.ctx, migrateAllLimit)
	if err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}
	if res.Deduped < 1 {
		t.Errorf("deduped %d, want at least the second identical blob", res.Deduped)
	}

	na := f.manifestID(t, a.ID)
	nb := f.manifestID(t, b.ID)
	if na == nil || nb == nil || *na != *nb {
		t.Error("identical blobs did not converge on one manifest after migration")
	}
}

func TestMigrateSkipsMissingBlob(t *testing.T) {
	// If the bytes are already gone, migration cannot invent them and must never
	// repoint the version at a manifest it could not build. It counts the version
	// as failed and leaves it blob-backed, where fsck reports the loss against the
	// format the operator can still reason about.
	f := newFixture(t)
	node := f.uploadBytes("gone.bin", uniqueData(40<<10, 7))

	// Delete the blob's bytes behind the database's back.
	fs := f.svc.Blobs().(*blob.FSStore)
	if err := fs.Delete(f.ctx, node.BlobKey); err != nil {
		t.Fatal(err)
	}

	f.enableCAS(t)
	res, err := f.svc.MigrateBlobs(f.ctx, migrateAllLimit)
	if err != nil {
		t.Fatalf("MigrateBlobs must not abort the pass on one missing blob: %v", err)
	}
	if res.Failed < 1 {
		t.Errorf("failed = %d, want at least this file's one", res.Failed)
	}

	// The version stays blob-backed: it was never repointed at a manifest built
	// from bytes that do not exist.
	if f.manifestID(t, node.ID) != nil {
		t.Error("version was repointed to a manifest built from missing bytes")
	}
}

func TestMigratableVersionsHonoursLimit(t *testing.T) {
	// Batch bounding lives in the candidate query's LIMIT — asserted here at the
	// store level, because the pass's own return count is global and unstable in
	// a shared database. Three of this fixture's own blobs guarantee the pool has
	// at least two candidates; a limit of two must never return more.
	f := newFixture(t)

	mine := map[uuid.UUID]bool{}
	for i := 0; i < 3; i++ {
		n := f.uploadBytes(uuid.NewString()+".bin", uniqueData(16<<10, int64(200+i)))
		mine[*n.HeadVersionID] = true
	}

	two, err := f.store.MigratableVersions(f.ctx, cas.WholeFileThreshold, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(two) != 2 {
		t.Errorf("limit=2 returned %d candidate(s), want exactly 2", len(two))
	}

	// And an unbounded listing must contain all three of this fixture's blobs —
	// proof the query selects blob-backed versions, not just that it caps them.
	all, err := f.store.MigratableVersions(f.ctx, cas.WholeFileThreshold, migrateAllLimit)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, v := range all {
		if mine[v.VersionID] {
			found++
		}
	}
	if found != 3 {
		t.Errorf("unbounded listing found %d of this fixture's 3 blobs", found)
	}
}

func TestMigratedFileSeeksForRange(t *testing.T) {
	// A blob served Range requests by seeking an *os.File; the chunked reader must
	// too, or migrating a video would break scrubbing that worked before. Prove a
	// mid-file read returns the right bytes after migration.
	f := newFixture(t)
	data := uniqueData(300<<10, 8)
	node := f.uploadBytes("clip.bin", data)

	f.enableCAS(t)
	if _, err := f.svc.MigrateBlobs(f.ctx, migrateAllLimit); err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}
	if f.manifestID(t, node.ID) == nil {
		t.Fatal("file was not migrated")
	}

	_, rc, err := f.svc.Open(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	const off, n = 128 << 10, 4096
	if _, err := rc.Seek(off, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got := make([]byte, n)
	if _, err := io.ReadFull(rc, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(got) != string(data[off:off+n]) {
		t.Error("seeked read of a migrated file returned the wrong bytes")
	}
}
