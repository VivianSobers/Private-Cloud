package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// Quota enforcement (Phase 9).
//
// The mechanism has existed since Phase 1 and the admin can set a limit since
// Phase 7; what Phase 9 owes is proof that the two meet — that a quota an
// administrator sets actually stops an upload, reports a status a client can act
// on, and is charged to the right account once sharing is in play.

// setQuota gives the fixture's main user a byte limit, as an admin would.
func (f *apiFixture) setQuota(t *testing.T, bytes int64) {
	t.Helper()
	rec := f.do(http.MethodPatch, "/api/v1/admin/users/"+f.userID.String(),
		jsonBody(t, map[string]any{"quota_bytes": bytes}), f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("set quota = %d: %s", rec.Code, rec.Body)
	}
}

func TestUploadIsRefusedOverQuota(t *testing.T) {
	f := newAPIFixture(t)
	root := f.root()
	f.setQuota(t, 32)

	if rec := f.upload(root, "small.txt", strings.Repeat("a", 16)); rec.Code != http.StatusCreated {
		t.Fatalf("upload inside the quota = %d: %s", rec.Code, rec.Body)
	}

	rec := f.upload(root, "big.txt", strings.Repeat("b", 1024))
	// 507, not 403: this is a storage condition, and clients that understand
	// WebDAV already read it as "stop, you are full".
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("upload over the quota = %d, want 507: %s", rec.Code, rec.Body)
	}
	if decode(t, rec)["error"].(map[string]any)["code"] != "quota_exceeded" {
		t.Error("the refusal does not carry a stable quota_exceeded code")
	}
}

// Trashed files still count. That is honest: the bytes really are still on the
// disk, and a quota that ignored them would let a user hold twice their limit by
// deleting nothing permanently.
func TestTrashedBytesStillCountTowardsQuota(t *testing.T) {
	f := newAPIFixture(t)
	root := f.root()
	f.setQuota(t, 64)

	id := nodeID(t, f.upload(root, "first.txt", strings.Repeat("a", 40)))
	if rec := f.json(http.MethodDelete, "/api/v1/nodes/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("trash = %d", rec.Code)
	}

	// The bytes are in the trash, not gone.
	if rec := f.upload(root, "second.txt", strings.Repeat("b", 40)); rec.Code != http.StatusInsufficientStorage {
		t.Errorf("upload after trashing = %d, want 507 — trashed bytes still occupy the disk", rec.Code)
	}

	usage := decode(t, f.json(http.MethodGet, "/api/v1/usage", nil))
	if usage["trash_bytes"].(float64) < 40 {
		t.Errorf("trash_bytes = %v, want at least the 40 trashed", usage["trash_bytes"])
	}
}

// Purging frees the quota, which is what makes the previous rule survivable.
func TestPurgingFreesQuota(t *testing.T) {
	f := newAPIFixture(t)
	root := f.root()
	f.setQuota(t, 64)

	id := nodeID(t, f.upload(root, "first.txt", strings.Repeat("a", 40)))
	if rec := f.json(http.MethodDelete, "/api/v1/nodes/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("trash = %d", rec.Code)
	}
	if rec := f.json(http.MethodDelete, "/api/v1/trash/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("purge = %d: %s", rec.Code, rec.Body)
	}

	if rec := f.upload(root, "second.txt", strings.Repeat("b", 40)); rec.Code != http.StatusCreated {
		t.Errorf("upload after purging = %d, want 201 — purging must free the quota", rec.Code)
	}
}

// No quota means unlimited, and clearing one restores that.
func TestClearingAQuotaRestoresUnlimited(t *testing.T) {
	f := newAPIFixture(t)
	root := f.root()
	f.setQuota(t, 8)

	if rec := f.upload(root, "over.txt", strings.Repeat("a", 64)); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("expected the quota to bite: %d", rec.Code)
	}

	rec := f.do(http.MethodPatch, "/api/v1/admin/users/"+f.userID.String(),
		jsonBody(t, map[string]any{"quota_bytes": nil}), f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear quota = %d: %s", rec.Code, rec.Body)
	}

	if rec := f.upload(root, "over.txt", strings.Repeat("a", 64)); rec.Code != http.StatusCreated {
		t.Errorf("upload after clearing the quota = %d, want 201", rec.Code)
	}
}

// The Phase 7 interaction: an editor writing into a shared folder spends the
// OWNER's quota, not their own. Otherwise sharing a folder is a way to spend
// somebody else's storage — or, just as bad, to have your own spent for you.
func TestEditorWritesAreChargedToTheOwnersQuota(t *testing.T) {
	f := newAPIFixture(t)
	folder := f.shareFolder(t, "shared-quota", "editor")
	f.setQuota(t, 32)

	rec := f.do(http.MethodPost,
		"/api/v1/upload?parent_id="+folder+"&name=big.txt",
		strings.NewReader(strings.Repeat("x", 512)), f.admin)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("editor upload into a full owner's folder = %d, want 507: %s", rec.Code, rec.Body)
	}
}

// usage reports the quota so a client can render a meter rather than discovering
// the limit by hitting it.
func TestUsageReportsTheQuota(t *testing.T) {
	f := newAPIFixture(t)
	f.setQuota(t, 4096)

	usage := decode(t, f.json(http.MethodGet, "/api/v1/usage", nil))
	if usage["quota_bytes"].(float64) != 4096 {
		t.Errorf("quota_bytes = %v, want 4096", usage["quota_bytes"])
	}
	if _, ok := usage["available_bytes"]; !ok {
		t.Error("no available_bytes — a client would have to compute the remainder itself")
	}
}
