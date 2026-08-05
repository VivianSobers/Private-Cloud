package files_test

// Slice 2: background migration of Phase 1 whole-file blobs into chunks.
//
// The scenario every test here reconstructs is the real one: content was
// uploaded before CAS existed (a blob-backed version), CAS is switched on, and
// the drain must rewrite that content into chunks WITHOUT the file it represents
// changing by a single byte, without touching quota, and without leaking the old
// blob. Both formats coexist, so a reader must not be able to tell which path a
// file took.

import (
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
)

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
	res, err := f.svc.MigrateBlobs(f.ctx, 100)
	if err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}
	if res.VersionsMigrated != 1 {
		t.Fatalf("migrated %d version(s), want 1", res.VersionsMigrated)
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
	if _, err := f.svc.MigrateBlobs(f.ctx, 100); err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}

	for _, it := range items {
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
	if _, err := f.svc.MigrateBlobs(f.ctx, 100); err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}
	if rc := f.blobRefcount(t, key); rc != 0 {
		t.Fatalf("blob refcount after migration = %d, want 0 — the UPDATE trigger did not fire", rc)
	}

	// With the reference gone, GC must reclaim the row and the bytes.
	f.svc.BlobGCGrace = 0
	gc, err := f.svc.CollectGarbage(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gc.BlobsFreed == 0 {
		t.Error("GC freed no blobs after migration unreferenced one")
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
	res, err := f.svc.MigrateBlobs(f.ctx, 100)
	if err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}
	if res.VersionsMigrated != 0 {
		t.Errorf("migrated %d sub-threshold file(s), want 0", res.VersionsMigrated)
	}

	got, err := f.store.Get(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ManifestID != nil {
		t.Error("a sub-threshold file was chunked by migration")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	// A second pass must find nothing: already-chunked versions are not
	// candidates, so re-running the drain — or a background loop overlapping a
	// manual run — is a no-op, never a double rewrite.
	f := newFixture(t)
	f.uploadBytes("once.bin", uniqueData(32<<10, 4))
	f.enableCAS(t)

	first, err := f.svc.MigrateBlobs(f.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if first.VersionsMigrated != 1 {
		t.Fatalf("first pass migrated %d, want 1", first.VersionsMigrated)
	}

	second, err := f.svc.MigrateBlobs(f.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if second.VersionsMigrated != 0 {
		t.Errorf("second pass migrated %d, want 0", second.VersionsMigrated)
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
	if _, err := f.svc.MigrateBlobs(f.ctx, 100); err != nil {
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
	res, err := f.svc.MigrateBlobs(f.ctx, 100)
	if err != nil {
		t.Fatalf("MigrateBlobs: %v", err)
	}
	if res.VersionsMigrated != 2 {
		t.Fatalf("migrated %d, want 2", res.VersionsMigrated)
	}
	if res.Deduped != 1 {
		t.Errorf("deduped %d, want 1 — the second blob should have reused the first's manifest", res.Deduped)
	}

	na, err := f.store.Get(f.ctx, f.user, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	nb, err := f.store.Get(f.ctx, f.user, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if na.ManifestID == nil || nb.ManifestID == nil || *na.ManifestID != *nb.ManifestID {
		t.Error("identical blobs did not converge on one manifest after migration")
	}
}
