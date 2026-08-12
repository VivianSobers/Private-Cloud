package files_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Albums are the one place a caller hands the server a list of node ids out of
// nowhere, so ownership is the property these tests care most about.

func TestAlbumCreateListAndDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	album, err := f.store.CreateAlbum(ctx, f.user, "Iceland 2026", "the good ones")
	if err != nil {
		t.Fatalf("create album: %v", err)
	}
	if album.Name != "Iceland 2026" || album.ItemCount != 0 {
		t.Fatalf("unexpected album: %+v", album)
	}

	albums, err := f.store.ListAlbums(ctx, f.user)
	if err != nil {
		t.Fatalf("list albums: %v", err)
	}
	if len(albums) != 1 || albums[0].ID != album.ID {
		t.Fatalf("list returned %d album(s), want the one just created", len(albums))
	}

	if err := f.store.DeleteAlbum(ctx, f.user, album.ID); err != nil {
		t.Fatalf("delete album: %v", err)
	}
	if _, err := f.store.GetAlbum(ctx, f.user, album.ID); !errors.Is(err, files.ErrAlbumNotFound) {
		t.Fatalf("after delete, get = %v, want ErrAlbumNotFound", err)
	}
}

func TestAlbumRejectsBlankName(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.CreateAlbum(context.Background(), f.user, "   ", ""); !errors.Is(err, files.ErrInvalidAlbumName) {
		t.Fatalf("create with blank name = %v, want ErrInvalidAlbumName", err)
	}
}

// Deleting an album must never delete the photos in it. This is the question
// every user has before they click the button, so it gets a test.
func TestDeletingAnAlbumKeepsItsFiles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	node := f.upload(f.root, "keep.txt", "still here")
	album, err := f.store.CreateAlbum(ctx, f.user, "Temporary", "")
	if err != nil {
		t.Fatalf("create album: %v", err)
	}
	if _, err := f.store.AddAlbumItems(ctx, f.user, album.ID, []uuid.UUID{node.ID}); err != nil {
		t.Fatalf("add item: %v", err)
	}
	if err := f.store.DeleteAlbum(ctx, f.user, album.ID); err != nil {
		t.Fatalf("delete album: %v", err)
	}

	if _, err := f.store.Get(ctx, f.user, node.ID); err != nil {
		t.Fatalf("file is gone after deleting the album that contained it: %v", err)
	}
}

// Adding preserves the caller's order, and re-adding is a no-op rather than a
// duplicate — which is what makes a retried request safe.
func TestAlbumItemsKeepOrderAndDedupe(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	a := f.upload(f.root, "a.txt", "a")
	b := f.upload(f.root, "b.txt", "b")
	c := f.upload(f.root, "c.txt", "c")

	album, err := f.store.CreateAlbum(ctx, f.user, "Ordered", "")
	if err != nil {
		t.Fatalf("create album: %v", err)
	}

	added, err := f.store.AddAlbumItems(ctx, f.user, album.ID, []uuid.UUID{c.ID, a.ID, b.ID})
	if err != nil {
		t.Fatalf("add items: %v", err)
	}
	if added != 3 {
		t.Fatalf("added = %d, want 3", added)
	}

	items, err := f.store.AlbumItems(ctx, f.user, album.ID, 100, 0)
	if err != nil {
		t.Fatalf("album items: %v", err)
	}
	if got := albumNames(items); got != "c.txt,a.txt,b.txt" {
		t.Fatalf("order = %s, want c.txt,a.txt,b.txt", got)
	}

	// Re-adding one that is already there adds nothing and reorders nothing.
	again, err := f.store.AddAlbumItems(ctx, f.user, album.ID, []uuid.UUID{a.ID})
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if again != 0 {
		t.Fatalf("re-adding an existing item added %d row(s), want 0", again)
	}
	items, _ = f.store.AlbumItems(ctx, f.user, album.ID, 100, 0)
	if got := albumNames(items); got != "c.txt,a.txt,b.txt" {
		t.Fatalf("order changed after a re-add: %s", got)
	}
}

func TestAlbumReorderReplacesTheWholeOrder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	a := f.upload(f.root, "a.txt", "a")
	b := f.upload(f.root, "b.txt", "b")
	c := f.upload(f.root, "c.txt", "c")

	album, _ := f.store.CreateAlbum(ctx, f.user, "Reorder", "")
	if _, err := f.store.AddAlbumItems(ctx, f.user, album.ID, []uuid.UUID{a.ID, b.ID, c.ID}); err != nil {
		t.Fatalf("add items: %v", err)
	}

	if err := f.store.ReorderAlbum(ctx, f.user, album.ID, []uuid.UUID{c.ID, b.ID, a.ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	items, _ := f.store.AlbumItems(ctx, f.user, album.ID, 100, 0)
	if got := albumNames(items); got != "c.txt,b.txt,a.txt" {
		t.Fatalf("order = %s, want c.txt,b.txt,a.txt", got)
	}
}

// The security property: a node id belonging to somebody else must not enter
// the album, and must not appear in its listing. Without the ownership filter in
// AddAlbumItems this is how one account would read another's file metadata.
func TestAlbumWillNotAcceptAnotherUsersNode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	mine := f.upload(f.root, "mine.txt", "mine")
	theirs := f.uploadAsOther(t, "theirs.txt")

	album, _ := f.store.CreateAlbum(ctx, f.user, "Mixed", "")
	added, err := f.store.AddAlbumItems(ctx, f.user, album.ID, []uuid.UUID{mine.ID, theirs.ID})
	if err != nil {
		t.Fatalf("add items: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1 — the other user's node must be skipped", added)
	}

	items, _ := f.store.AlbumItems(ctx, f.user, album.ID, 100, 0)
	if got := albumNames(items); got != "mine.txt" {
		t.Fatalf("album contains %s, want only mine.txt", got)
	}
}

// An album is not reachable by a user who does not own it, and the error does
// not distinguish "not yours" from "does not exist".
func TestAlbumIsNotVisibleToAnotherUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	album, _ := f.store.CreateAlbum(ctx, f.user, "Private", "")

	if _, err := f.store.GetAlbum(ctx, f.other(t), album.ID); !errors.Is(err, files.ErrAlbumNotFound) {
		t.Fatalf("other user's get = %v, want ErrAlbumNotFound", err)
	}
	if err := f.store.DeleteAlbum(ctx, f.other(t), album.ID); !errors.Is(err, files.ErrAlbumNotFound) {
		t.Fatalf("other user's delete = %v, want ErrAlbumNotFound", err)
	}
	if _, err := f.store.AlbumItems(ctx, f.other(t), album.ID, 10, 0); !errors.Is(err, files.ErrAlbumNotFound) {
		t.Fatalf("other user's items = %v, want ErrAlbumNotFound", err)
	}
}

// A cover has to be one of the caller's own files. The column is only a foreign
// key to nodes, so without the check an album tile could render a stranger's
// photo.
func TestAlbumCoverMustBeOwned(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	theirs := f.uploadAsOther(t, "theirs.txt")
	album, _ := f.store.CreateAlbum(ctx, f.user, "Cover", "")

	cover := &theirs.ID
	_, err := f.store.UpdateAlbum(ctx, f.user, album.ID, files.AlbumPatch{CoverNodeID: &cover})
	if !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("setting another user's node as cover = %v, want ErrNotFound", err)
	}
}

// Patch semantics: an absent field is left alone, and an explicit empty cover
// clears it. A struct of plain strings could not tell those apart.
func TestAlbumPatchLeavesAbsentFieldsAlone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	album, _ := f.store.CreateAlbum(ctx, f.user, "Original", "a description")

	newName := "Renamed"
	updated, err := f.store.UpdateAlbum(ctx, f.user, album.ID, files.AlbumPatch{Name: &newName})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if updated.Name != "Renamed" {
		t.Errorf("name = %q, want Renamed", updated.Name)
	}
	if updated.Description != "a description" {
		t.Errorf("description = %q — an absent field must be left alone", updated.Description)
	}

	node := f.upload(f.root, "cover.txt", "x")
	cover := &node.ID
	if _, err := f.store.UpdateAlbum(ctx, f.user, album.ID, files.AlbumPatch{CoverNodeID: &cover}); err != nil {
		t.Fatalf("set cover: %v", err)
	}
	var cleared *uuid.UUID
	after, err := f.store.UpdateAlbum(ctx, f.user, album.ID, files.AlbumPatch{CoverNodeID: &cleared})
	if err != nil {
		t.Fatalf("clear cover: %v", err)
	}
	if after.CoverNodeID != nil {
		t.Errorf("cover = %v, want cleared", after.CoverNodeID)
	}
}

// Purging a file must remove it from every album it was in; a dangling item
// would render as a broken tile with no way to clear it.
func TestPurgingAFileRemovesItFromAlbums(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	node := f.upload(f.root, "doomed.txt", "bye")
	album, _ := f.store.CreateAlbum(ctx, f.user, "Doomed", "")
	if _, err := f.store.AddAlbumItems(ctx, f.user, album.ID, []uuid.UUID{node.ID}); err != nil {
		t.Fatalf("add item: %v", err)
	}

	if _, err := f.store.Trash(ctx, f.user, node.ID); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if err := f.store.Purge(ctx, f.user, node.ID); err != nil {
		t.Fatalf("purge: %v", err)
	}

	after, err := f.store.GetAlbum(ctx, f.user, album.ID)
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if after.ItemCount != 0 {
		t.Fatalf("item_count = %d after purging its only file, want 0", after.ItemCount)
	}
}

func albumNames(nodes []*files.Node) string {
	out := ""
	for i, n := range nodes {
		if i > 0 {
			out += ","
		}
		out += n.Name
	}
	return out
}
