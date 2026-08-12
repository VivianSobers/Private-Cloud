package httpapi_test

import (
	"net/http"
	"testing"
)

// The admin console.
//
// Two properties carry most of the weight: every route is refused to a
// non-admin server-side (the client's nav gating is convenience, not the
// boundary), and DELETE disables rather than destroys.

func TestAdminRoutesRefuseNonAdmins(t *testing.T) {
	f := newAPIFixture(t)

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/users"},
		{http.MethodPost, "/api/v1/admin/users"},
		{http.MethodPatch, "/api/v1/admin/users/" + f.userID.String()},
		{http.MethodDelete, "/api/v1/admin/users/" + f.userID.String()},
		{http.MethodGet, "/api/v1/admin/users/" + f.userID.String() + "/sessions"},
		{http.MethodGet, "/api/v1/admin/audit"},
	} {
		// f.cookie is the ordinary, non-admin account.
		rec := f.do(c.method, c.path, nil, f.cookie)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a non-admin = %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

func TestAdminListsUsersWithUsage(t *testing.T) {
	f := newAPIFixture(t)
	f.upload(f.root(), "a.txt", "0123456789")

	rec := f.do(http.MethodGet, "/api/v1/admin/users", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list users = %d: %s", rec.Code, rec.Body)
	}
	users := decode(t, rec)["users"].([]any)
	if len(users) < 2 {
		t.Fatalf("got %d user(s), want at least the two the fixture makes", len(users))
	}

	var found bool
	for _, raw := range users {
		u := raw.(map[string]any)
		if u["username"] != f.username {
			continue
		}
		found = true
		if u["used_bytes"].(float64) < 10 {
			t.Errorf("used_bytes = %v, want at least the 10 bytes uploaded", u["used_bytes"])
		}
		// Absent means unlimited. A zero would be a quota of zero bytes, which is
		// the opposite of what no quota means.
		if _, ok := u["quota_bytes"]; ok {
			t.Error("a user with no quota reported a quota_bytes value")
		}
	}
	if !found {
		t.Error("the uploading user is missing from the admin list")
	}
}

func TestAdminCreatesUserWithRecoveryCodes(t *testing.T) {
	f := newAPIFixture(t)

	name := "provisioned-" + f.username
	rec := f.do(http.MethodPost, "/api/v1/admin/users",
		jsonBody(t, map[string]any{"username": name, "quota_bytes": 1024}), f.admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user = %d: %s", rec.Code, rec.Body)
	}

	body := decode(t, rec)
	// A new account has no passkey, so recovery codes are the only way in. This
	// response is the one time they exist in plain text.
	codes := body["recovery_codes"].([]any)
	if len(codes) == 0 {
		t.Fatal("no recovery codes returned — the account would be unreachable")
	}
	user := body["user"].(map[string]any)
	if user["username"] != name {
		t.Errorf("username = %v, want %v", user["username"], name)
	}
	if user["quota_bytes"].(float64) != 1024 {
		t.Errorf("quota_bytes = %v, want 1024", user["quota_bytes"])
	}

	// And the name is taken now.
	rec = f.do(http.MethodPost, "/api/v1/admin/users",
		jsonBody(t, map[string]any{"username": name}), f.admin)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate username = %d, want 409", rec.Code)
	}
}

// quota_bytes distinguishes "absent" from "present and null" — the two are
// opposite instructions, and a plain pointer cannot tell them apart.
func TestAdminQuotaPatchDistinguishesAbsentFromNull(t *testing.T) {
	f := newAPIFixture(t)
	id := f.userID.String()

	set := func(body map[string]any) map[string]any {
		t.Helper()
		rec := f.do(http.MethodPatch, "/api/v1/admin/users/"+id, jsonBody(t, body), f.admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %v = %d: %s", body, rec.Code, rec.Body)
		}
		return decode(t, rec)["user"].(map[string]any)
	}

	if u := set(map[string]any{"quota_bytes": 5000}); u["quota_bytes"].(float64) != 5000 {
		t.Fatalf("quota = %v, want 5000", u["quota_bytes"])
	}
	// Absent: leave it alone.
	if u := set(map[string]any{"display_name": "Renamed"}); u["quota_bytes"].(float64) != 5000 {
		t.Errorf("an absent quota_bytes changed the quota to %v", u["quota_bytes"])
	}
	// Present and null: clear it.
	if u := set(map[string]any{"quota_bytes": nil}); u["quota_bytes"] != nil {
		t.Errorf("an explicit null did not clear the quota: %v", u["quota_bytes"])
	}
}

// Disabling has to bite immediately. An account whose browser tab keeps working
// is not disabled.
func TestDisablingAUserRevokesTheirSessionsAtOnce(t *testing.T) {
	f := newAPIFixture(t)

	if rec := f.do(http.MethodGet, "/api/v1/auth/me", nil, f.cookie); rec.Code != http.StatusOK {
		t.Fatalf("user is not signed in to begin with: %d", rec.Code)
	}

	rec := f.do(http.MethodDelete, "/api/v1/admin/users/"+f.userID.String(), nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", rec.Code, rec.Body)
	}

	if rec := f.do(http.MethodGet, "/api/v1/auth/me", nil, f.cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("a disabled user's session still works: %d", rec.Code)
	}
}

// DELETE disables; it must not destroy content. "Remove this person's access"
// almost never means "destroy everything they ever uploaded".
func TestDisablingAUserKeepsTheirFiles(t *testing.T) {
	f := newAPIFixture(t)
	id := nodeID(t, f.upload(f.root(), "keep.txt", "still here"))

	if rec := f.do(http.MethodDelete, "/api/v1/admin/users/"+f.userID.String(), nil, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d", rec.Code)
	}

	var count int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM nodes WHERE id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("count node: %v", err)
	}
	if count != 1 {
		t.Error("disabling a user destroyed their files")
	}
}

// Demotion works while another admin exists, and it bites immediately: the
// demoted account loses the admin plane on its very next request.
//
// The refusal side of this rule — that the LAST enabled admin cannot be demoted
// or disabled — is a global condition over the users table, and these tests
// share a database in which every fixture creates another admin. It is covered
// in the auth package, which can construct the condition; asserting it here
// would either never fire or require disabling accounts other tests are using.
func TestDemotingAnAdminTakesEffectAtOnce(t *testing.T) {
	f := newAPIFixture(t)

	// A fresh admin to demote, so no other test's fixture is disturbed.
	rec := f.do(http.MethodPost, "/api/v1/admin/users",
		jsonBody(t, map[string]any{"username": "demoted-" + f.username, "is_admin": true}), f.admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create admin = %d: %s", rec.Code, rec.Body)
	}
	id := decode(t, rec)["user"].(map[string]any)["id"].(string)

	rec = f.do(http.MethodPatch, "/api/v1/admin/users/"+id,
		jsonBody(t, map[string]any{"is_admin": false}), f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("demote = %d: %s", rec.Code, rec.Body)
	}
	if decode(t, rec)["user"].(map[string]any)["is_admin"] != false {
		t.Error("is_admin is still true after demotion")
	}
}

// Grants are authorisation-relevant, so they are in the log. Reads are not.
func TestAuditLogRecordsGrants(t *testing.T) {
	f := newAPIFixture(t)
	id := nodeID(t, f.upload(f.root(), "doc.txt", "doc"))

	if rec := f.json(http.MethodPost, "/api/v1/nodes/"+id+"/grants",
		map[string]any{"username": f.adminUsername, "role": "viewer"}); rec.Code != http.StatusCreated {
		t.Fatalf("grant = %d: %s", rec.Code, rec.Body)
	}

	rec := f.do(http.MethodGet, "/api/v1/admin/audit?action=grant.create", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d: %s", rec.Code, rec.Body)
	}
	entries := decode(t, rec)["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("granting produced no audit entry")
	}

	var found bool
	for _, raw := range entries {
		e := raw.(map[string]any)
		if e["actor"] == f.username && e["action"] == "grant.create" {
			found = true
			if e["request_id"] == "" {
				t.Error("audit entry has no request_id to tie it to the access log")
			}
			detail := e["detail"].(map[string]any)
			if detail["grantee"] != f.adminUsername || detail["role"] != "viewer" {
				t.Errorf("detail = %v, want the grantee and role", detail)
			}
		}
	}
	if !found {
		t.Error("no grant.create entry by the granting user")
	}
}

// adminUserID looks up the fixture's admin account id through the admin list.
func (f *apiFixture) adminUserID(t *testing.T) string {
	t.Helper()
	rec := f.do(http.MethodGet, "/api/v1/admin/users", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list users = %d", rec.Code)
	}
	for _, raw := range decode(t, rec)["users"].([]any) {
		u := raw.(map[string]any)
		if u["username"] == f.adminUsername {
			return u["id"].(string)
		}
	}
	t.Fatal("admin account not found in the user list")
	return ""
}
