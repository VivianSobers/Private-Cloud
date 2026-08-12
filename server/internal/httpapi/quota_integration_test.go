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
// TestAdminUsageMatchesEnforcement pins the two numbers together.
//
// The admin column read LiveBytes while checkQuota counted live, trashed and
// retained bytes, so an admin could watch an account refused at its limit with
// the column beside that limit reading well under it. A number labelled "used"
// that is not the number enforcement uses is worse than no number: it makes the
// refusal look like a bug in the server rather than a full disk.
func TestAdminUsageMatchesEnforcement(t *testing.T) {
	f := newAPIFixture(t)

	// Something live, something trashed, and a superseded version — one of each
	// thing the quota counts and the old column did not.
	f.upload(f.root(), "kept.txt", strings.Repeat("k", 300))
	f.upload(f.root(), "kept.txt", strings.Repeat("K", 500)) // supersedes the above
	binned := nodeID(t, f.upload(f.root(), "binned.txt", strings.Repeat("b", 700)))
	if rec := f.do(http.MethodDelete, "/api/v1/nodes/"+binned, nil, f.cookie); rec.Code != http.StatusOK {
		t.Fatalf("trash = %d", rec.Code)
	}

	usage := decode(t, f.json(http.MethodGet, "/api/v1/usage", nil))
	total := usage["total_bytes"].(float64)

	users := decode(t, f.do(http.MethodGet, "/api/v1/admin/users", nil, f.admin))["users"].([]any)
	var found bool
	for _, raw := range users {
		u := raw.(map[string]any)
		if u["username"] != f.username {
			continue
		}
		found = true
		if u["used_bytes"].(float64) != total {
			t.Errorf("admin used_bytes = %v, /usage total_bytes = %v — two definitions of used",
				u["used_bytes"], total)
		}
		// And the parts are there, so the total can be explained.
		for _, part := range []string{"live_bytes", "trash_bytes", "version_bytes"} {
			if _, ok := u[part]; !ok {
				t.Errorf("admin user listing is missing %q", part)
			}
		}
	}
	if !found {
		t.Fatal("the fixture's user was not in the admin listing")
	}
}

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
