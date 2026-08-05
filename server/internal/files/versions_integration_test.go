package files_test

// Slice 3: version history — list, restore, retention.
//
// Phase 1 already wrote a row per overwrite and kept them all; it simply never
// exposed the history or bounded it. These tests pin the three things that make
// it real: the list reads newest-first with the head flagged, restore is an
// APPEND that never destroys the versions rolled past, and pruning bounds the
// history by both count and age while never touching the head.

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// overwrite re-uploads the same name, which PutFile records as a new version and
// moves the head to — the ordinary way history accrues.
func (f *fixture) overwrite(name, content string) *files.Node {
	f.t.Helper()
	return f.upload(f.root, name, content)
}

// backdate rewrites a version's created_at so a retention test does not have to
// wait 90 days. White-box, but the alternative is a sleep the length of the
// policy window.
func (f *fixture) backdate(t *testing.T, versionID uuid.UUID, age time.Duration) {
	t.Helper()
	_, err := f.store.Pool().Exec(f.ctx,
		`UPDATE file_versions SET created_at = now() - $2::interval WHERE id = $1`,
		versionID, age.String())
	if err != nil {
		t.Fatalf("backdate version: %v", err)
	}
}

func TestListVersionsNewestFirstHeadFlagged(t *testing.T) {
	f := newFixture(t)
	f.overwrite("doc.txt", "one")
	f.overwrite("doc.txt", "two")
	node := f.overwrite("doc.txt", "three")

	versions, err := f.store.ListVersions(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(versions))
	}

	// Newest first, and exactly one head — the newest.
	if !versions[0].IsHead {
		t.Error("the newest version is not flagged as head")
	}
	for i := 1; i < len(versions); i++ {
		if versions[i].IsHead {
			t.Errorf("version %d (not newest) is flagged as head", i)
		}
		if versions[i].CreatedAt.After(versions[i-1].CreatedAt) {
			t.Error("versions are not ordered newest-first")
		}
	}
}

func TestListVersionsUnknownNodeNotFound(t *testing.T) {
	// A file always has at least one version, so an empty history means the node
	// does not exist — reported as not-found, not an empty list a client would
	// misread as "a file with no versions".
	f := newFixture(t)
	if _, err := f.store.ListVersions(f.ctx, f.user, uuid.New()); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("ListVersions of a missing node = %v, want ErrNotFound", err)
	}
}

func TestListVersionsRejectsForeignOwner(t *testing.T) {
	// Ownership is in the WHERE clause: another user's id must not turn a node id
	// into a cross-tenant history dump.
	f := newFixture(t)
	node := f.overwrite("secret.txt", "mine")

	if _, err := f.store.ListVersions(f.ctx, uuid.New(), node.ID); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("ListVersions as a foreign owner = %v, want ErrNotFound", err)
	}
}

func TestRestoreVersionAppendsNewHead(t *testing.T) {
	// Restore rolls content back by ADDING a version, never by deleting the ones
	// in between — so the rollback is itself undoable.
	f := newFixture(t)
	f.overwrite("note.txt", "first draft")
	node := f.overwrite("note.txt", "second draft")

	versions, err := f.store.ListVersions(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	// versions[1] is the older "first draft".
	oldest := versions[len(versions)-1]

	restored, err := f.store.RestoreVersion(f.ctx, f.user, node.ID, oldest.ID)
	if err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}

	// The head now serves the old content...
	if got := string(f.readBack(restored.ID)); got != "first draft" {
		t.Errorf("restored head reads %q, want %q", got, "first draft")
	}
	// ...and history GREW rather than shrank: the two originals plus the restore.
	after, err := f.store.ListVersions(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 3 {
		t.Errorf("history has %d versions after restore, want 3 (nothing deleted)", len(after))
	}
	if !after[0].IsHead {
		t.Error("the restore did not become the head")
	}
}

func TestRestoreRejectsForeignVersion(t *testing.T) {
	// A version id belonging to another file must not be graftable onto this one:
	// the restore query is scoped to (version, node) together.
	f := newFixture(t)
	a := f.overwrite("a.txt", "aaa")
	b := f.overwrite("b.txt", "bbb")

	bVersions, err := f.store.ListVersions(f.ctx, f.user, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RestoreVersion(f.ctx, f.user, a.ID, bVersions[0].ID); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("restoring another file's version = %v, want ErrNotFound", err)
	}
}

func TestOpenVersionServesHistoricalBytes(t *testing.T) {
	// Downloading a past version must serve THAT snapshot, not the head — the UI
	// previews an old version without restoring it first. The reader also seeks,
	// so ServeContent can answer a Range request against history.
	f := newFixture(t)
	old := "the original text, long enough to seek within"
	f.overwrite("story.txt", old)
	node := f.overwrite("story.txt", "the replacement text")

	versions, err := f.store.ListVersions(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldest := versions[len(versions)-1]

	vc, rc, err := f.svc.OpenVersion(f.ctx, f.user, node.ID, oldest.ID)
	if err != nil {
		t.Fatalf("OpenVersion: %v", err)
	}
	defer rc.Close()

	if vc.Name != "story.txt" {
		t.Errorf("version download name = %q, want story.txt", vc.Name)
	}
	if got, _ := io.ReadAll(rc); string(got) != old {
		t.Errorf("version content = %q, want %q", got, old)
	}

	// Seek mid-way and read the tail — Range support against a past version.
	if _, err := rc.Seek(4, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	tail := make([]byte, 8)
	if _, err := io.ReadFull(rc, tail); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(tail) != old[4:12] {
		t.Errorf("seeked read = %q, want %q", tail, old[4:12])
	}
}

func TestPruneKeepsVersionsWithinRetention(t *testing.T) {
	// Anything younger than the retention window survives whatever its rank: a
	// burst of edits in one afternoon is not history to throw away, even past the
	// keep-count. Fresh versions here, an aggressive keepN, a real window — none
	// should be pruned. Scoped to this file's own history, because PruneVersions
	// is global and the shared database holds other fixtures' versions.
	f := newFixture(t)
	f.overwrite("live.txt", "a")
	f.overwrite("live.txt", "b")
	node := f.overwrite("live.txt", "c")

	if _, err := f.store.PruneVersions(f.ctx, 1, 90*24*time.Hour, 1000); err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}

	after, err := f.store.ListVersions(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 3 {
		t.Errorf("recent history pruned: %d versions remain, want 3", len(after))
	}
}

func TestPruneDropsAgedExcess(t *testing.T) {
	// A version pruned only when it fails BOTH tests: beyond the keep-count AND
	// older than the window. Backdate the two non-head versions past the window,
	// keep just one, and they go — the head, newest and within the window, stays.
	f := newFixture(t)
	f.overwrite("old.txt", "v1")
	f.overwrite("old.txt", "v2")
	node := f.overwrite("old.txt", "v3")

	versions, err := f.store.ListVersions(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		if !v.IsHead {
			f.backdate(t, v.ID, 200*24*time.Hour)
		}
	}

	pruned, err := f.store.PruneVersions(f.ctx, 1, 90*24*time.Hour, 1000)
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	if pruned < 2 {
		t.Errorf("pruned %d, want at least this file's 2 aged versions", pruned)
	}

	after, err := f.store.ListVersions(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || !after[0].IsHead {
		t.Fatalf("want only the head left, got %d versions", len(after))
	}
	if got := string(f.readBack(node.ID)); got != "v3" {
		t.Errorf("surviving head reads %q, want v3", got)
	}
}

func TestPruneNeverDropsHead(t *testing.T) {
	// The head is protected by id, not just by rank — so even a head made older
	// than a sibling (which normal operations never produce, but a corrupt
	// created_at could) survives. Backdate the head PAST a non-head version and
	// past the window, then prune to a single kept version: the head must remain.
	f := newFixture(t)
	f.overwrite("h.txt", "before")
	node := f.overwrite("h.txt", "after")

	versions, err := f.store.ListVersions(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		if v.IsHead {
			f.backdate(t, v.ID, 300*24*time.Hour) // older than its sibling
		} else {
			f.backdate(t, v.ID, 100*24*time.Hour)
		}
	}

	if _, err := f.store.PruneVersions(f.ctx, 1, 90*24*time.Hour, 1000); err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}

	after, err := f.store.ListVersions(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	var headSeen bool
	for _, v := range after {
		if v.IsHead {
			headSeen = true
		}
	}
	if !headSeen {
		t.Error("the head was pruned despite being older than a sibling")
	}
	if got := string(f.readBack(node.ID)); got != "after" {
		t.Errorf("head content = %q, want after", got)
	}
}
