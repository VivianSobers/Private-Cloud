package files_test

import (
	"errors"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Grants are the phase's authorisation model, so these tests are mostly about
// what must NOT happen. AccessFor is the single question every shared read goes
// through; if it is wrong, every endpoint built on it is wrong the same way.

func TestOwnerAlwaysHasOwnerAccess(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "mine.txt", "mine")

	acc, err := f.store.AccessFor(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatalf("AccessFor: %v", err)
	}
	if acc.Role != files.RoleOwner {
		t.Errorf("role = %q, want owner", acc.Role)
	}
	if acc.Shared {
		t.Error("a node the caller owns must not be marked shared — absence of access is what means mine")
	}
	if !acc.CanWrite() {
		t.Error("owner cannot write")
	}
}

// No grant, no access — and the answer is indistinguishable from "no such node",
// so an unauthorised caller cannot use it to confirm an id is real.
func TestStrangerHasNoAccessAndCannotTellWhy(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "private.txt", "secret")

	_, err := f.store.AccessFor(f.ctx, f.other(t), node.ID)
	if !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("stranger's AccessFor = %v, want ErrNotFound", err)
	}
}

func TestGrantGivesReadAccess(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)
	node := f.upload(f.root, "shared.txt", "hello")

	if _, err := f.store.CreateGrant(f.ctx, f.user, node.ID, other, files.RoleViewer); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	acc, err := f.store.AccessFor(f.ctx, other, node.ID)
	if err != nil {
		t.Fatalf("AccessFor after grant: %v", err)
	}
	if acc.Role != files.RoleViewer {
		t.Errorf("role = %q, want viewer", acc.Role)
	}
	if !acc.Shared {
		t.Error("a granted node must be marked shared")
	}
	if acc.CanWrite() {
		t.Error("a viewer must not be able to write")
	}
}

// The inheritance property: a folder grant covers files created AFTER it, which
// is the whole reason inheritance is derived from the path instead of expanded
// into rows at grant time.
func TestFolderGrantCoversFilesCreatedLater(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)

	folder := f.mkdir(f.root, "project")
	if _, err := f.store.CreateGrant(f.ctx, f.user, folder.ID, other, files.RoleEditor); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	// Created after the grant, and nested two levels down.
	sub := f.mkdir(folder.ID, "notes")
	later := f.upload(sub.ID, "later.txt", "written after the share")

	acc, err := f.store.AccessFor(f.ctx, other, later.ID)
	if err != nil {
		t.Fatalf("AccessFor on a later descendant: %v", err)
	}
	if acc.Role != files.RoleEditor {
		t.Errorf("role = %q, want editor inherited from the folder", acc.Role)
	}
}

// A sibling of a shared folder must not be reachable. This is the test that
// fails if the path prefix is compared without the separator — "/projectX"
// starts with "/project".
func TestFolderGrantDoesNotLeakToSiblings(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)

	shared := f.mkdir(f.root, "project")
	if _, err := f.store.CreateGrant(f.ctx, f.user, shared.ID, other, files.RoleViewer); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	// Same prefix, different folder.
	sibling := f.mkdir(f.root, "projectX")
	secret := f.upload(sibling.ID, "secret.txt", "not shared")

	if _, err := f.store.AccessFor(f.ctx, other, secret.ID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("a grant on /project leaked into /projectX: %v", err)
	}
}

// A folder whose name contains LIKE metacharacters must grant access to itself
// and nothing else.
func TestFolderGrantEscapesLikeMetacharacters(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)

	odd := f.mkdir(f.root, "100%_done")
	if _, err := f.store.CreateGrant(f.ctx, f.user, odd.ID, other, files.RoleViewer); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	inside := f.upload(odd.ID, "in.txt", "in")

	// The grant works for its own subtree.
	if _, err := f.store.AccessFor(f.ctx, other, inside.ID); err != nil {
		t.Fatalf("grant on a %%/_ named folder does not cover its own contents: %v", err)
	}

	// And a folder the wildcard would have matched is untouched.
	decoy := f.mkdir(f.root, "1009Xdone")
	hidden := f.upload(decoy.ID, "hidden.txt", "hidden")
	if _, err := f.store.AccessFor(f.ctx, other, hidden.ID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("LIKE metacharacters in a folder name widened the grant: %v", err)
	}
}

// Two grants covering the same node give the union of what they allow, not
// whichever happens to sort first.
func TestEditorWinsOverViewer(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)

	folder := f.mkdir(f.root, "docs")
	file := f.upload(folder.ID, "spec.txt", "spec")

	if _, err := f.store.CreateGrant(f.ctx, f.user, folder.ID, other, files.RoleViewer); err != nil {
		t.Fatalf("folder grant: %v", err)
	}
	if _, err := f.store.CreateGrant(f.ctx, f.user, file.ID, other, files.RoleEditor); err != nil {
		t.Fatalf("file grant: %v", err)
	}

	acc, err := f.store.AccessFor(f.ctx, other, file.ID)
	if err != nil {
		t.Fatalf("AccessFor: %v", err)
	}
	if acc.Role != files.RoleEditor {
		t.Errorf("role = %q, want editor — access from two grants is the union", acc.Role)
	}
}

// Only the owner may grant. An editor re-sharing would spread access
// transitively beyond what the owner can see or revoke.
func TestEditorCannotReshare(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)
	node := f.upload(f.root, "doc.txt", "doc")

	if _, err := f.store.CreateGrant(f.ctx, f.user, node.ID, other, files.RoleEditor); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	// `other` is an editor and now tries to grant the same node onward.
	third := f.third(t)
	if _, err := f.store.CreateGrant(f.ctx, other, node.ID, third, files.RoleViewer); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("an editor re-shared a file: %v", err)
	}
	if _, err := f.store.AccessFor(f.ctx, third, node.ID); !errors.Is(err, files.ErrNotFound) {
		t.Fatal("a third party gained access through an editor's re-share")
	}
}

// Regranting is a role change, not a duplicate.
func TestRegrantingChangesTheRole(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)
	node := f.upload(f.root, "doc.txt", "doc")

	if _, err := f.store.CreateGrant(f.ctx, f.user, node.ID, other, files.RoleViewer); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	if _, err := f.store.CreateGrant(f.ctx, f.user, node.ID, other, files.RoleEditor); err != nil {
		t.Fatalf("regrant: %v", err)
	}

	grants, err := f.store.GrantsForNode(f.ctx, f.user, node.ID)
	if err != nil {
		t.Fatalf("GrantsForNode: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("got %d grants, want 1 — regranting must not duplicate", len(grants))
	}
	if grants[0].Role != files.RoleEditor {
		t.Errorf("role = %q, want editor", grants[0].Role)
	}
}

func TestRevokingAGrantIsImmediate(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)
	node := f.upload(f.root, "doc.txt", "doc")

	g, err := f.store.CreateGrant(f.ctx, f.user, node.ID, other, files.RoleViewer)
	if err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	if err := f.store.DeleteGrant(f.ctx, f.user, g.ID); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	if _, err := f.store.AccessFor(f.ctx, other, node.ID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("access survived revocation: %v", err)
	}
}

// A grantee may decline a share. Without this they have no way to clear
// somebody else's folder out of their own "shared with me".
func TestGranteeCanDeclineAShare(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)
	node := f.upload(f.root, "doc.txt", "doc")

	g, err := f.store.CreateGrant(f.ctx, f.user, node.ID, other, files.RoleViewer)
	if err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	if err := f.store.DeleteGrant(f.ctx, other, g.ID); err != nil {
		t.Fatalf("grantee could not decline: %v", err)
	}
}

// But an unrelated third party may not revoke somebody else's grant.
func TestStrangerCannotRevokeAGrant(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)
	node := f.upload(f.root, "doc.txt", "doc")

	g, err := f.store.CreateGrant(f.ctx, f.user, node.ID, other, files.RoleViewer)
	if err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	if err := f.store.DeleteGrant(f.ctx, f.third(t), g.ID); !errors.Is(err, files.ErrGrantNotFound) {
		t.Fatalf("a stranger revoked a grant: %v", err)
	}
}

func TestGrantRejectsSelfAndBadRoles(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "doc.txt", "doc")

	if _, err := f.store.CreateGrant(f.ctx, f.user, node.ID, f.user, files.RoleViewer); !errors.Is(err, files.ErrCannotGrantToSelf) {
		t.Errorf("granting to self = %v, want ErrCannotGrantToSelf", err)
	}
	// 'owner' is a real role but not a grantable one — it would be a second,
	// contradictory claim about who owns the node.
	for _, role := range []string{"owner", "admin", ""} {
		if _, err := f.store.CreateGrant(f.ctx, f.user, node.ID, f.other(t), role); !errors.Is(err, files.ErrInvalidRole) {
			t.Errorf("role %q = %v, want ErrInvalidRole", role, err)
		}
	}
}

// "Shared with me" lists the roots granted, not every covered descendant — a
// folder share would otherwise dump somebody else's whole tree into the view.
func TestSharedRootsListsRootsOnly(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)

	folder := f.mkdir(f.root, "project")
	f.upload(folder.ID, "a.txt", "a")
	f.upload(folder.ID, "b.txt", "b")
	if _, err := f.store.CreateGrant(f.ctx, f.user, folder.ID, other, files.RoleViewer); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	nodes, grants, err := f.store.SharedRoots(f.ctx, other)
	if err != nil {
		t.Fatalf("SharedRoots: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != folder.ID {
		t.Fatalf("got %d root(s), want just the shared folder", len(nodes))
	}
	if len(grants) != 1 || grants[0].Role != files.RoleViewer {
		t.Fatalf("grants = %+v, want one viewer grant", grants)
	}
}

// Trashing a shared folder withdraws access rather than leaving it dangling.
func TestTrashingASharedNodeWithdrawsAccess(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)

	folder := f.mkdir(f.root, "project")
	file := f.upload(folder.ID, "a.txt", "a")
	if _, err := f.store.CreateGrant(f.ctx, f.user, folder.ID, other, files.RoleViewer); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	if _, err := f.store.Trash(f.ctx, f.user, folder.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	if _, err := f.store.AccessFor(f.ctx, other, file.ID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("access survived the shared folder being trashed: %v", err)
	}
}

// The batch resolver has to agree with the single one, or a listing will show
// different permissions from the detail view of the same file.
func TestAccessForNodesAgreesWithAccessFor(t *testing.T) {
	f := newFixture(t)
	other := f.other(t)

	folder := f.mkdir(f.root, "project")
	shared := f.upload(folder.ID, "shared.txt", "shared")
	unshared := f.upload(f.root, "unshared.txt", "unshared")
	if _, err := f.store.CreateGrant(f.ctx, f.user, folder.ID, other, files.RoleEditor); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	nodes := []*files.Node{shared, unshared}
	batch, err := f.store.AccessForNodes(f.ctx, other, nodes)
	if err != nil {
		t.Fatalf("AccessForNodes: %v", err)
	}

	if got, ok := batch[shared.ID]; !ok || got.Role != files.RoleEditor {
		t.Errorf("batch access for the shared file = %+v, want editor", got)
	}
	if _, ok := batch[unshared.ID]; ok {
		t.Error("the batch resolver returned access for a node with no grant")
	}

	single, err := f.store.AccessFor(f.ctx, other, shared.ID)
	if err != nil {
		t.Fatalf("AccessFor: %v", err)
	}
	if single.Role != batch[shared.ID].Role {
		t.Errorf("single = %q, batch = %q — the two resolvers disagree",
			single.Role, batch[shared.ID].Role)
	}
}
