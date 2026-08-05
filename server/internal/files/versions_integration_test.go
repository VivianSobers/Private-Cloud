package files_test

// Slice 3: version history — list, restore, retention.
//
// Phase 1 already wrote a row per overwrite and kept them all; it simply never
// exposed the history or bounded it. These tests pin the three things that make
// it real: the list reads newest-first with the head flagged, restore is an
// APPEND that never destroys the versions rolled past, and pruning bounds the
// history by both count and age while never touching the head.

import (
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
