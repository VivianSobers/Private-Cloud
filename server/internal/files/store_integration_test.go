package files_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// These tests need a real Postgres. The tree logic lives almost entirely in
// SQL — partial unique indexes, cascades, the refcount trigger, prefix
// rewrites on rename — and none of that is exercised by a mock. A fake store
// would pass while the real schema silently allowed duplicate siblings.
//
// Run with:
//
//	PC_TEST_DATABASE_URL=postgres://... go test ./internal/files/...
func testDB(t *testing.T) *db.DB {
	t.Helper()

	dsn := os.Getenv("PC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PC_TEST_DATABASE_URL not set; skipping integration tests")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	database, err := db.Open(ctx, dsn, 8, 1, 10*time.Second, log)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(database.Close)

	if err := database.Migrate(ctx, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return database
}

// newFixture gives each test its own user, so tests never collide on the
// per-owner root or on sibling names.
type fixture struct {
	t     *testing.T
	ctx   context.Context
	store *files.Store
	svc   *files.Service
	user  uuid.UUID
	root  uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	database := testDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	authStore := auth.NewStore(database.Pool)
	username := "test-" + uuid.NewString()[:8]
	user, err := authStore.CreateUser(ctx, uuid.New(), username, username, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	blobs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	store := files.NewStore(database.Pool)
	root, err := store.EnsureRoot(ctx, user.ID)
	if err != nil {
		t.Fatalf("ensure root: %v", err)
	}

	return &fixture{
		t: t, ctx: ctx, store: store,
		svc:  files.NewService(store, blobs, log),
		user: user.ID, root: root.ID,
	}
}

func (f *fixture) mkdir(parent uuid.UUID, name string) *files.Node {
	f.t.Helper()
	n, err := f.store.CreateFolder(f.ctx, f.user, parent, name)
	if err != nil {
		f.t.Fatalf("CreateFolder(%q): %v", name, err)
	}
	return n
}

func (f *fixture) upload(parent uuid.UUID, name, content string) *files.Node {
	f.t.Helper()
	n, err := f.svc.Upload(f.ctx, f.user, parent, name, strings.NewReader(content), "")
	if err != nil {
		f.t.Fatalf("Upload(%q): %v", name, err)
	}
	return n
}

// --- tree -------------------------------------------------------------------

func TestEnsureRootIsIdempotent(t *testing.T) {
	f := newFixture(t)

	// The partial unique index on (owner_id) WHERE parent_id IS NULL is what
	// makes the concurrent case safe; this asserts the sequential one.
	for i := 0; i < 3; i++ {
		got, err := f.store.EnsureRoot(f.ctx, f.user)
		if err != nil {
			t.Fatalf("EnsureRoot: %v", err)
		}
		if got.ID != f.root {
			t.Fatalf("EnsureRoot returned a different root on call %d", i)
		}
		if got.Path != "/" {
			t.Errorf("root path = %q, want /", got.Path)
		}
	}
}

func TestSiblingNamesAreCaseInsensitive(t *testing.T) {
	// macOS and Windows clients will cheerfully try to create "Photos" beside
	// "photos". Allowing both produces a pair of entries only some clients can
	// see, which is worse than refusing the second.
	f := newFixture(t)
	f.mkdir(f.root, "Photos")

	if _, err := f.store.CreateFolder(f.ctx, f.user, f.root, "photos"); err == nil {
		t.Fatal("created photos beside Photos; the folded uniqueness index is not working")
	} else if !strings.Contains(err.Error(), files.ErrNameTaken.Error()) {
		t.Fatalf("got %v, want ErrNameTaken", err)
	}

	// Different folders may each hold a "photos".
	other := f.mkdir(f.root, "other")
	f.mkdir(other.ID, "photos")
}

func TestPathsAreMaterialised(t *testing.T) {
	f := newFixture(t)
	a := f.mkdir(f.root, "a")
	b := f.mkdir(a.ID, "b")
	file := f.upload(b.ID, "note.txt", "hello")

	if a.Path != "/a" || b.Path != "/a/b" || file.Path != "/a/b/note.txt" {
		t.Fatalf("paths wrong: %q %q %q", a.Path, b.Path, file.Path)
	}

	got, err := f.store.GetByPath(f.ctx, f.user, "/a/b/note.txt")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got.ID != file.ID {
		t.Error("GetByPath resolved to the wrong node")
	}
}

func TestRenameRewritesDescendantPaths(t *testing.T) {
	// This is the cost of materialising paths, and the thing most likely to be
	// forgotten. A descendant whose path no longer matches its parent chain
	// makes every prefix query return the wrong subtree.
	f := newFixture(t)
	a := f.mkdir(f.root, "old")
	b := f.mkdir(a.ID, "inner")
	file := f.upload(b.ID, "deep.txt", "x")

	if _, err := f.store.MoveOrRename(f.ctx, f.user, a.ID, uuid.Nil, "new"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	for id, want := range map[uuid.UUID]string{
		a.ID:    "/new",
		b.ID:    "/new/inner",
		file.ID: "/new/inner/deep.txt",
	} {
		got, err := f.store.Get(f.ctx, f.user, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Path != want {
			t.Errorf("path = %q, want %q", got.Path, want)
		}
	}
}

func TestRenameDoesNotTouchSimilarlyNamedSiblings(t *testing.T) {
	// "/photos" and "/photos-backup" share a textual prefix. A prefix rewrite
	// that forgets the separator would drag the second along with the first.
	f := newFixture(t)
	photos := f.mkdir(f.root, "photos")
	backup := f.mkdir(f.root, "photos-backup")
	inner := f.mkdir(backup.ID, "inner")

	if _, err := f.store.MoveOrRename(f.ctx, f.user, photos.ID, uuid.Nil, "pictures"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got, err := f.store.Get(f.ctx, f.user, inner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/photos-backup/inner" {
		t.Errorf("unrelated sibling was rewritten: path = %q", got.Path)
	}
}

func TestRenameWithLikeWildcardsInName(t *testing.T) {
	// A folder genuinely named with % or _ must not turn the descendant
	// rewrite into a wildcard match over unrelated paths.
	f := newFixture(t)
	weird := f.mkdir(f.root, "100%_done")
	child := f.mkdir(weird.ID, "child")
	bystander := f.mkdir(f.root, "1000xxdone")
	bystanderChild := f.mkdir(bystander.ID, "kid")

	if _, err := f.store.MoveOrRename(f.ctx, f.user, weird.ID, uuid.Nil, "finished"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got, _ := f.store.Get(f.ctx, f.user, child.ID)
	if got.Path != "/finished/child" {
		t.Errorf("child path = %q, want /finished/child", got.Path)
	}
	got, _ = f.store.Get(f.ctx, f.user, bystanderChild.ID)
	if got.Path != "/1000xxdone/kid" {
		t.Errorf("LIKE wildcard leaked: bystander path = %q", got.Path)
	}
}

func TestMoveIntoOwnSubtreeIsRejected(t *testing.T) {
	// Allowing it would detach the subtree from the root entirely: the rows
	// survive, nothing reaches them, and the paths become self-referential.
	f := newFixture(t)
	a := f.mkdir(f.root, "a")
	b := f.mkdir(a.ID, "b")

	if _, err := f.store.MoveOrRename(f.ctx, f.user, a.ID, b.ID, ""); err == nil {
		t.Fatal("moved a folder into its own descendant")
	}
	if _, err := f.store.MoveOrRename(f.ctx, f.user, a.ID, a.ID, ""); err == nil {
		t.Fatal("moved a folder into itself")
	}
}

func TestRootCannotBeRenamedOrTrashed(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.MoveOrRename(f.ctx, f.user, f.root, uuid.Nil, "nope"); err == nil {
		t.Error("renamed the root")
	}
	if _, err := f.store.Trash(f.ctx, f.user, f.root); err == nil {
		t.Error("trashed the root")
	}
}

func TestCrossTenantAccessIsImpossible(t *testing.T) {
	// Ownership is in the WHERE clause, not an assertion afterwards. This
	// asserts the query, because a handler that forgot the check would still
	// pass a test written against a mock.
	a := newFixture(t)
	b := newFixture(t)

	secret := a.upload(a.root, "secret.txt", "private")

	if _, err := b.store.Get(b.ctx, b.user, secret.ID); err == nil {
		t.Fatal("another user read a node by id")
	}
	if _, err := b.store.Trash(b.ctx, b.user, secret.ID); err == nil {
		t.Fatal("another user trashed a node by id")
	}
	if _, err := b.store.MoveOrRename(b.ctx, b.user, secret.ID, b.root, "stolen.txt"); err == nil {
		t.Fatal("another user moved a node into their own tree")
	}
}

// --- content ----------------------------------------------------------------

func TestUploadAndReadBack(t *testing.T) {
	f := newFixture(t)
	const content = "the quick brown fox"

	node := f.upload(f.root, "fox.txt", content)
	if node.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", node.Size, len(content))
	}
	want := sha256.Sum256([]byte(content))
	if fmt.Sprintf("%x", node.SHA256) != fmt.Sprintf("%x", want) {
		t.Error("stored hash does not match the content")
	}
	if !strings.HasPrefix(node.MIME, "text/plain") {
		t.Errorf("mime = %q, want text/plain", node.MIME)
	}

	_, rc, err := f.svc.Open(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	if string(got) != content {
		t.Errorf("read back %q, want %q", got, content)
	}
}

func TestUploadOverExistingFileReplacesContent(t *testing.T) {
	// Uploading over an existing file is what every client expects, and WebDAV's
	// PUT semantics require it.
	f := newFixture(t)
	first := f.upload(f.root, "notes.txt", "version one")
	second := f.upload(f.root, "notes.txt", "version two, longer")

	if first.ID != second.ID {
		t.Error("re-upload created a second node instead of a new version")
	}
	if second.Size != int64(len("version two, longer")) {
		t.Errorf("size = %d, not updated to the new version", second.Size)
	}

	_, rc, err := f.svc.Open(f.ctx, f.user, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "version two, longer" {
		t.Errorf("read %q, want the new version", got)
	}

	children, _ := f.store.ListChildren(f.ctx, f.user, f.root)
	if len(children) != 1 {
		t.Errorf("root has %d children, want 1", len(children))
	}
}

func TestUploadOntoAFolderNameIsRejected(t *testing.T) {
	// Silently replacing it would delete a subtree the user never asked to
	// delete.
	f := newFixture(t)
	f.mkdir(f.root, "docs")

	if _, err := f.svc.Upload(f.ctx, f.user, f.root, "docs", strings.NewReader("x"), ""); err == nil {
		t.Fatal("uploaded a file over a folder")
	}
}

func TestEmptyFileRoundTrips(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "empty.txt", "")
	if node.Size != 0 {
		t.Errorf("size = %d, want 0", node.Size)
	}
	_, rc, err := f.svc.Open(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatalf("Open on an empty file: %v", err)
	}
	rc.Close()
}

// --- trash ------------------------------------------------------------------

func TestTrashHidesSubtreeAndFreesTheName(t *testing.T) {
	f := newFixture(t)
	folder := f.mkdir(f.root, "project")
	f.upload(folder.ID, "a.txt", "a")
	f.upload(folder.ID, "b.txt", "b")

	affected, err := f.store.Trash(f.ctx, f.user, folder.ID)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if affected != 3 {
		t.Errorf("trashed %d nodes, want 3 (the folder and both files)", affected)
	}

	children, _ := f.store.ListChildren(f.ctx, f.user, f.root)
	if len(children) != 0 {
		t.Errorf("root still lists %d children after trashing", len(children))
	}

	// Deleting "project" must not block creating a new one.
	f.mkdir(f.root, "project")

	// Only the node the user actually deleted appears in the trash — showing
	// all its contents would make the trash unusable.
	trash, err := f.store.ListTrash(f.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	if len(trash) != 1 || trash[0].ID != folder.ID {
		t.Errorf("trash listing = %d item(s), want just the deleted folder", len(trash))
	}
}

func TestRestoreBringsBackTheWholeSubtree(t *testing.T) {
	f := newFixture(t)
	folder := f.mkdir(f.root, "project")
	file := f.upload(folder.ID, "a.txt", "a")

	if _, err := f.store.Trash(f.ctx, f.user, folder.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Restore(f.ctx, f.user, folder.ID); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := f.store.GetLive(f.ctx, f.user, file.ID)
	if err != nil {
		t.Fatalf("file did not come back: %v", err)
	}
	if got.Path != "/project/a.txt" {
		t.Errorf("restored path = %q", got.Path)
	}
}

func TestRestoreRefusesNonTrashRoots(t *testing.T) {
	// Restoring a file from inside a deleted folder would leave a live node
	// under a trashed parent, which no other query is prepared for.
	f := newFixture(t)
	folder := f.mkdir(f.root, "project")
	file := f.upload(folder.ID, "a.txt", "a")

	if _, err := f.store.Trash(f.ctx, f.user, folder.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Restore(f.ctx, f.user, file.ID); err == nil {
		t.Fatal("restored a node from inside a deleted folder")
	}
}

func TestRestoreConflictsWithARetakenName(t *testing.T) {
	f := newFixture(t)
	original := f.upload(f.root, "notes.txt", "original")
	if _, err := f.store.Trash(f.ctx, f.user, original.ID); err != nil {
		t.Fatal(err)
	}
	f.upload(f.root, "notes.txt", "replacement")

	if _, err := f.store.Restore(f.ctx, f.user, original.ID); err == nil {
		t.Fatal("restore silently clobbered or duplicated the retaken name")
	}
}

func TestIndividuallyDeletedFilesDoNotComeBackWithTheFolder(t *testing.T) {
	// The reason trashed_root_id exists. Without it, a subtree deleted as one
	// unit is indistinguishable from its members having been deleted
	// separately beforehand.
	f := newFixture(t)
	folder := f.mkdir(f.root, "project")
	keep := f.upload(folder.ID, "keep.txt", "keep")
	discard := f.upload(folder.ID, "discard.txt", "discard")

	if _, err := f.store.Trash(f.ctx, f.user, discard.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Trash(f.ctx, f.user, folder.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Restore(f.ctx, f.user, folder.ID); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if _, err := f.store.GetLive(f.ctx, f.user, keep.ID); err != nil {
		t.Errorf("keep.txt did not come back: %v", err)
	}
	if _, err := f.store.GetLive(f.ctx, f.user, discard.ID); err == nil {
		t.Error("discard.txt was resurrected; it had been deleted separately")
	}
}

// --- quota, refcounts and GC ------------------------------------------------

func TestUsageCountsTrashSeparately(t *testing.T) {
	// A quota that ignored the trash would let a user store twice their
	// allowance by never emptying it.
	f := newFixture(t)
	live := f.upload(f.root, "live.txt", "12345")
	gone := f.upload(f.root, "gone.txt", "1234567890")

	if _, err := f.store.Trash(f.ctx, f.user, gone.ID); err != nil {
		t.Fatal(err)
	}

	usage, err := f.store.Usage(f.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	if usage.LiveBytes != live.Size {
		t.Errorf("live bytes = %d, want %d", usage.LiveBytes, live.Size)
	}
	if usage.TrashBytes != gone.Size {
		t.Errorf("trash bytes = %d, want %d", usage.TrashBytes, gone.Size)
	}
	if usage.TotalBytes() != live.Size+gone.Size {
		t.Errorf("total = %d, want %d", usage.TotalBytes(), live.Size+gone.Size)
	}
}

func TestPurgeDropsRefcountAndGCReclaimsBytes(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "temp.bin", "some bytes here")

	// The blob is still referenced, so it must survive a GC pass.
	f.svc.BlobGCGrace = 0
	if _, err := f.svc.CollectGarbage(f.ctx); err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if _, _, err := f.svc.Open(f.ctx, f.user, node.ID); err != nil {
		t.Fatalf("GC deleted a referenced blob: %v", err)
	}

	if _, err := f.store.Trash(f.ctx, f.user, node.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Purge(f.ctx, f.user, node.ID); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	// The trigger should have taken the refcount to zero when the cascade
	// removed the version row.
	res, err := f.svc.CollectGarbage(f.ctx)
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if res.BlobsFreed != 1 {
		t.Errorf("GC freed %d blobs, want 1 — the refcount trigger did not fire on cascade", res.BlobsFreed)
	}
	if res.BytesFreed != node.Size {
		t.Errorf("GC freed %d bytes, want %d", res.BytesFreed, node.Size)
	}
}

func TestOverwritingAFileLeavesTheOldBlobCollectable(t *testing.T) {
	f := newFixture(t)
	f.upload(f.root, "notes.txt", "the first version")
	f.upload(f.root, "notes.txt", "the second version")

	// Slice 3 keeps no history, so the first version's blob is now unreferenced
	// once its version row is gone. The version row itself survives (history
	// lands in Phase 2), so nothing should be collectable yet.
	f.svc.BlobGCGrace = 0
	res, err := f.svc.CollectGarbage(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.BlobsFreed != 0 {
		t.Errorf("GC freed %d blobs; superseded versions are retained, not collected", res.BlobsFreed)
	}
}

func TestFsckFindsOrphansAndMissingContent(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "real.txt", "real content")

	// An orphan: bytes on disk that no row references. This is what a crash
	// between writing the blob and committing the transaction leaves behind.
	fs := f.svc.Blobs().(*blob.FSStore)
	orphan, err := fs.Put(f.ctx, strings.NewReader("orphaned bytes"))
	if err != nil {
		t.Fatal(err)
	}

	// Missing is asserted by membership, not by count. fsck compares the whole
	// blobs table against one blob store — correct in production, where there
	// is exactly one store, but here every other test's fixture has its own
	// temp directory and its rows are legitimately "missing" from this one.
	report, err := f.svc.Fsck(f.ctx, false)
	if err != nil {
		t.Fatalf("Fsck: %v", err)
	}
	if len(report.Orphans) != 1 || report.Orphans[0] != orphan.Key {
		t.Errorf("orphans = %v, want just %q", report.Orphans, orphan.Key)
	}
	if contains(report.Missing, node.BlobKey) {
		t.Errorf("a live file's content was reported missing: %q", node.BlobKey)
	}

	// Repair removes the orphan and leaves the real file alone.
	if _, err := f.svc.Fsck(f.ctx, true); err != nil {
		t.Fatalf("Fsck(repair): %v", err)
	}
	if _, _, err := f.svc.Open(f.ctx, f.user, node.ID); err != nil {
		t.Fatalf("repair deleted a live file: %v", err)
	}

	report, err = f.svc.Fsck(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Orphans) != 0 {
		t.Errorf("orphans survived repair: %v", report.Orphans)
	}

	// Now the other direction: a row whose bytes have vanished. fsck must
	// report it rather than quietly deleting the record of a file the user
	// still expects to exist.
	live, err := f.store.Get(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(f.ctx, live.BlobKey); err != nil {
		t.Fatal(err)
	}
	report, err = f.svc.Fsck(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(report.Missing, live.BlobKey) {
		t.Errorf("deleted blob %q was not reported missing", live.BlobKey)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestFailedUploadLeavesNoBlobBehind(t *testing.T) {
	// The bytes go down before the row, so a failed transaction must clean up
	// after itself — otherwise every rejected upload leaks disk until GC runs.
	f := newFixture(t)
	f.mkdir(f.root, "occupied")

	if _, err := f.svc.Upload(f.ctx, f.user, f.root, "occupied", strings.NewReader("rejected"), ""); err == nil {
		t.Fatal("upload onto a folder name succeeded")
	}

	report, err := f.svc.Fsck(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Orphans) != 0 {
		t.Errorf("failed upload left %d orphan blob(s) behind", len(report.Orphans))
	}
}

func TestAutoPurgeRespectsRetention(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "old.txt", "content")
	if _, err := f.store.Trash(f.ctx, f.user, node.ID); err != nil {
		t.Fatal(err)
	}

	// Freshly trashed: a 30-day retention must leave it alone.
	n, err := f.store.AutoPurgeTrash(f.ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("auto-purge removed %d item(s) inside the retention window", n)
	}
	if _, err := f.store.Get(f.ctx, f.user, node.ID); err != nil {
		t.Errorf("node was purged early: %v", err)
	}

	// Expire THIS test's node by backdating it, never by shrinking the
	// retention to zero. AutoPurgeTrash is global across owners — in
	// production there is one server and that is correct — so a zero
	// retention here purges every OTHER fixture's freshly trashed rows too.
	// Test packages run as parallel binaries against one shared database, and
	// this exact call was deleting internal/httpapi's WebDAV-test trash in
	// the window between its DELETE and its trash listing.
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE nodes SET trashed_at = trashed_at - interval '31 days' WHERE id = $1`,
		node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AutoPurgeTrash(f.ctx, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Get(f.ctx, f.user, node.ID); err == nil {
		t.Error("expired trash survived auto-purge")
	}
}

func TestEmptyTrashPurgesEverything(t *testing.T) {
	f := newFixture(t)
	a := f.upload(f.root, "a.txt", "a")
	b := f.mkdir(f.root, "b")
	f.upload(b.ID, "inner.txt", "inner")

	for _, id := range []uuid.UUID{a.ID, b.ID} {
		if _, err := f.store.Trash(f.ctx, f.user, id); err != nil {
			t.Fatal(err)
		}
	}

	n, err := f.store.EmptyTrash(f.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("EmptyTrash purged %d trash root(s), want 2", n)
	}

	trash, _ := f.store.ListTrash(f.ctx, f.user)
	if len(trash) != 0 {
		t.Errorf("trash still holds %d item(s)", len(trash))
	}

	usage, _ := f.store.Usage(f.ctx, f.user)
	if usage.TotalBytes() != 0 {
		t.Errorf("usage after emptying trash = %d bytes, want 0", usage.TotalBytes())
	}
}

func TestQuotaIsEnforced(t *testing.T) {
	f := newFixture(t)

	var quota int64 = 20
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE users SET quota_bytes = $2 WHERE id = $1`, f.user, quota); err != nil {
		t.Fatal(err)
	}

	f.upload(f.root, "a.bin", strings.Repeat("x", 15))

	_, err := f.svc.Upload(f.ctx, f.user, f.root, "b.bin", strings.NewReader(strings.Repeat("y", 10)), "")
	if err == nil {
		t.Fatal("upload exceeding the quota succeeded")
	}
	if !strings.Contains(err.Error(), files.ErrQuota.Error()) {
		t.Fatalf("got %v, want ErrQuota", err)
	}

	// And the rejected upload must not have left its bytes on disk.
	report, _ := f.svc.Fsck(f.ctx, false)
	if len(report.Orphans) != 0 {
		t.Errorf("quota-rejected upload left %d orphan(s)", len(report.Orphans))
	}
}
