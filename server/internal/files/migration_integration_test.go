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
