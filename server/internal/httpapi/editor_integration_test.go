package httpapi_test

import (
	"net/http"
	"testing"
)

// What an editor grant actually permits, and — more importantly — what it does
// not. "Reads and writes" has to mean writes that land in the owner's tree and
// on the owner's quota, or sharing a folder becomes a way to spend somebody
// else's storage.

// shareFolder creates a folder owned by the fixture's main user and grants the
// admin account a role on it, returning the folder id.
func (f *apiFixture) shareFolder(t *testing.T, name, role string) string {
	t.Helper()
	folder := nodeID(t, f.json(http.MethodPost, "/api/v1/folders",
		map[string]any{"parent_id": f.root(), "name": name}))
	rec := f.json(http.MethodPost, "/api/v1/nodes/"+folder+"/grants",
		map[string]any{"username": f.adminUsername, "role": role})
	if rec.Code != http.StatusCreated {
		t.Fatalf("grant %s = %d: %s", role, rec.Code, rec.Body)
	}
	return folder
}

// An editor's upload belongs to the OWNER, not to the editor. A grant never
// moves or copies anything, so a file created in a shared folder is the owner's,
// exactly as if they had created it themselves.
func TestEditorUploadIsOwnedAndChargedToTheOwner(t *testing.T) {
	f := newAPIFixture(t)
	folder := f.shareFolder(t, "shared-editable", "editor")

	rec := f.do(http.MethodPost,
		"/api/v1/upload?parent_id="+folder+"&name=from-editor.txt",
		jsonBody(t, "ignored"), f.admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("editor upload = %d: %s", rec.Code, rec.Body)
	}
	id := nodeID(t, rec)

	var ownerID string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT owner_id::text FROM nodes WHERE id = $1`, id).Scan(&ownerID); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if ownerID != f.userID.String() {
		t.Errorf("owner_id = %s, want the folder owner %s", ownerID, f.userID)
	}

	// And it counts against the owner's usage, not the editor's.
	usage := decode(t, f.json(http.MethodGet, "/api/v1/usage", nil))
	if usage["file_count"].(float64) < 1 {
		t.Error("the editor's upload did not count towards the owner's usage")
	}
	editorUsage := decode(t, f.do(http.MethodGet, "/api/v1/usage", nil, f.admin))
	if editorUsage["file_count"].(float64) != 0 {
		t.Errorf("the editor was charged %v file(s) for writing into somebody else's folder",
			editorUsage["file_count"])
	}
}

// A viewer may read and must not write.
func TestViewerCannotWrite(t *testing.T) {
	f := newAPIFixture(t)
	folder := f.shareFolder(t, "shared-readonly", "viewer")

	rec := f.do(http.MethodPost,
		"/api/v1/upload?parent_id="+folder+"&name=nope.txt",
		jsonBody(t, "ignored"), f.admin)
	if rec.Code != http.StatusNotFound {
		t.Errorf("viewer upload = %d, want 404", rec.Code)
	}

	rec = f.do(http.MethodPost, "/api/v1/folders",
		jsonBody(t, map[string]any{"parent_id": folder, "name": "nope"}), f.admin)
	if rec.Code != http.StatusNotFound {
		t.Errorf("viewer folder create = %d, want 404", rec.Code)
	}
}

// A stranger with no grant at all writes nowhere.
func TestStrangerCannotWriteIntoAnotherTree(t *testing.T) {
	f := newAPIFixture(t)
	folder := nodeID(t, f.json(http.MethodPost, "/api/v1/folders",
		map[string]any{"parent_id": f.root(), "name": "private"}))

	rec := f.do(http.MethodPost,
		"/api/v1/upload?parent_id="+folder+"&name=intruder.txt",
		jsonBody(t, "ignored"), f.admin)
	if rec.Code != http.StatusNotFound {
		t.Errorf("stranger upload = %d, want 404", rec.Code)
	}
}

// An editor may rename inside the shared folder.
func TestEditorCanRenameInsideASharedFolder(t *testing.T) {
	f := newAPIFixture(t)
	folder := f.shareFolder(t, "shared-rename", "editor")
	id := nodeID(t, f.upload(folder, "before.txt", "x"))

	rec := f.do(http.MethodPatch, "/api/v1/nodes/"+id,
		jsonBody(t, map[string]any{"name": "after.txt"}), f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("editor rename = %d: %s", rec.Code, rec.Body)
	}
	if decode(t, rec)["node"].(map[string]any)["name"] != "after.txt" {
		t.Error("rename did not take")
	}
}

// But an editor must not be able to move a shared file into their OWN tree.
// That would be a copy the owner never agreed to and a silent transfer of the
// bytes onto the editor's quota.
func TestEditorCannotMoveASharedFileIntoTheirOwnTree(t *testing.T) {
	f := newAPIFixture(t)
	folder := f.shareFolder(t, "shared-move", "editor")
	id := nodeID(t, f.upload(folder, "wanted.txt", "x"))

	// The editor's own root.
	editorRoot := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.admin))

	rec := f.do(http.MethodPatch, "/api/v1/nodes/"+id,
		jsonBody(t, map[string]any{"parent_id": editorRoot}), f.admin)
	if rec.Code == http.StatusOK {
		t.Fatal("an editor moved a shared file into their own tree")
	}

	var ownerID string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT owner_id::text FROM nodes WHERE id = $1`, id).Scan(&ownerID); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if ownerID != f.userID.String() {
		t.Errorf("owner changed to %s", ownerID)
	}
}

// An editor's delete goes to the OWNER's trash, so the owner can restore it.
func TestEditorDeleteGoesToTheOwnersTrash(t *testing.T) {
	f := newAPIFixture(t)
	folder := f.shareFolder(t, "shared-delete", "editor")
	id := nodeID(t, f.upload(folder, "doomed.txt", "x"))

	if rec := f.do(http.MethodDelete, "/api/v1/nodes/"+id, nil, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("editor delete = %d: %s", rec.Code, rec.Body)
	}

	// The owner can see and restore it.
	items := decode(t, f.json(http.MethodGet, "/api/v1/trash", nil))["items"].([]any)
	var found bool
	for _, raw := range items {
		if raw.(map[string]any)["id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatal("an editor's delete did not land in the owner's trash")
	}
	if rec := f.json(http.MethodPost, "/api/v1/trash/"+id+"/restore", nil); rec.Code != http.StatusOK {
		t.Errorf("owner could not restore what the editor deleted: %d", rec.Code)
	}
}

// Writing into somebody else's tree is authorisation-relevant and logged;
// writing into your own is not, or the log would drown in ordinary uploads.
func TestEditorWritesAreAudited(t *testing.T) {
	f := newAPIFixture(t)
	folder := f.shareFolder(t, "shared-audited", "editor")

	if rec := f.do(http.MethodPost,
		"/api/v1/upload?parent_id="+folder+"&name=logged.txt",
		jsonBody(t, "ignored"), f.admin); rec.Code != http.StatusCreated {
		t.Fatalf("editor upload = %d", rec.Code)
	}

	entries := decode(t, f.do(http.MethodGet,
		"/api/v1/admin/audit?action=node.create", nil, f.admin))["entries"].([]any)
	var found bool
	for _, raw := range entries {
		if e := raw.(map[string]any); e["actor"] == f.adminUsername {
			found = true
		}
	}
	if !found {
		t.Error("an editor's write into another tree produced no audit entry")
	}

	// The owner's own uploads are NOT logged.
	f.upload(f.root(), "ordinary.txt", "x")
	entries = decode(t, f.do(http.MethodGet,
		"/api/v1/admin/audit?action=node.create", nil, f.admin))["entries"].([]any)
	for _, raw := range entries {
		if e := raw.(map[string]any); e["actor"] == f.username {
			t.Error("an ordinary upload into the caller's own tree was audited")
		}
	}
}
