package httpapi_test

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/zeebo/blake3"
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

// TestEditorWritesOnEveryUploadPath is the same rule as above, asked of the
// paths Phase 7 never reached.
//
// POST /upload was made owner-aware and the other two writes were not, so
// whether an editor could write depended on which transport the client picked —
// and the web client picks by SIZE. An editor could drop a 5 MiB file into a
// shared folder and got a 404 for a 9 MiB one. The sync client, which commits
// every file it uploads through /manifests, could not write to a shared folder
// at all.
func TestEditorWritesOnEveryUploadPath(t *testing.T) {
	f := newAPIFixture(t)
	folder := f.shareFolder(t, "shared-every-path", "editor")

	ownerOf := func(id string) string {
		t.Helper()
		var owner string
		if err := f.pool.QueryRow(f.ctx,
			`SELECT owner_id::text FROM nodes WHERE id = $1`, id).Scan(&owner); err != nil {
			t.Fatalf("read owner: %v", err)
		}
		return owner
	}

	t.Run("resumable", func(t *testing.T) {
		const body = "resumable payload from an editor"

		req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Length", strconv.Itoa(len(body)))
		req.Header.Set("Upload-Metadata", meta(map[string]string{
			"filename": "from-editor.bin", "parent_id": folder,
		}))
		req.AddCookie(f.admin)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("editor tus create = %d: %s", rec.Code, rec.Body)
		}
		loc := rec.Header().Get("Location")

		req = httptest.NewRequest(http.MethodPatch, loc, strings.NewReader(body))
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.Header.Set("Upload-Offset", "0")
		req.AddCookie(f.admin)
		rec = httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("editor tus patch = %d: %s", rec.Code, rec.Body)
		}

		// The finished file is the folder owner's, even though the session was
		// the editor's — the session is who resumes, the file is whose tree it
		// lands in, and those are deliberately different questions.
		id := nodeID(t, f.json(http.MethodGet, "/api/v1/nodes/resolve?path=%2Fshared-every-path%2Ffrom-editor.bin", nil))
		if got := ownerOf(id); got != f.userID.String() {
			t.Errorf("resumable upload owner = %s, want the folder owner %s", got, f.userID)
		}
	})

	t.Run("manifest commit", func(t *testing.T) {
		data := []byte(uuid.NewString() + " committed by an editor")
		sum := blake3.Sum256(data)
		h := hex.EncodeToString(sum[:])

		if rec := f.do(http.MethodPut, "/api/v1/chunks/"+h,
			bytes.NewReader(data), f.admin); rec.Code != http.StatusCreated {
			t.Fatalf("editor put chunk = %d", rec.Code)
		}
		rec := f.do(http.MethodPost,
			"/api/v1/manifests?parent_id="+folder+"&name=committed.bin",
			jsonBody(t, map[string]any{"content_hash": h, "chunks": []string{h}}), f.admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("editor manifest commit = %d: %s", rec.Code, rec.Body)
		}
		if got := ownerOf(nodeID(t, rec)); got != f.userID.String() {
			t.Errorf("manifest commit owner = %s, want the folder owner %s", got, f.userID)
		}
	})
}

// TestViewerCannotWriteOnAnyUploadPath is the refusal half. A read-only grant
// that could be escalated by choosing a different transport would not be a
// read-only grant.
func TestViewerCannotWriteOnAnyUploadPath(t *testing.T) {
	f := newAPIFixture(t)
	folder := f.shareFolder(t, "shared-readonly-paths", "viewer")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "4")
	req.Header.Set("Upload-Metadata", meta(map[string]string{
		"filename": "nope.bin", "parent_id": folder,
	}))
	req.AddCookie(f.admin)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("viewer tus create = %d, want 404", rec.Code)
	}

	data := []byte(uuid.NewString() + " viewer bytes")
	sum := blake3.Sum256(data)
	h := hex.EncodeToString(sum[:])
	if rec := f.do(http.MethodPut, "/api/v1/chunks/"+h,
		bytes.NewReader(data), f.admin); rec.Code != http.StatusCreated {
		t.Fatalf("put chunk = %d", rec.Code)
	}
	rec = f.do(http.MethodPost,
		"/api/v1/manifests?parent_id="+folder+"&name=nope.bin",
		jsonBody(t, map[string]any{"content_hash": h, "chunks": []string{h}}), f.admin)
	if rec.Code != http.StatusNotFound {
		t.Errorf("viewer manifest commit = %d, want 404", rec.Code)
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
