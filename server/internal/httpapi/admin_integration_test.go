package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
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

// The refusal side of the last-admin guard, over the real router.
//
// "Exactly one enabled administrator" is a global property of the users table,
// not of a fixture, and every fixture mints a second admin — which is why this
// case went untested for a phase. The condition is CONSTRUCTED here instead:
// every other enabled admin is disabled directly in the database for the length
// of the test, leaving the fixture's own admin as the only one, and restored
// afterwards. That is safe because testdb gives this test binary a database of
// its own and Go runs these tests one at a time (none of them calls t.Parallel),
// so no other test is looking at the users table while this one holds it.
//
// The guard itself is untouched: what is asserted is the shipped 409 and the
// shipped error code, through the same handlers a console would call. Anything
// weaker — a hook, a flag, a test-only bypass — would be testing the scaffolding
// rather than the rule that keeps an operator from locking themselves out.
func TestLastAdminCannotBeDemotedOrDisabled(t *testing.T) {
	f := newAPIFixture(t)

	var adminID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM users WHERE username = $1`, f.adminUsername).Scan(&adminID); err != nil {
		t.Fatalf("look up the fixture admin: %v", err)
	}

	// Stand every other enabled admin down, and put them back afterwards. Rows
	// are collected before the update so the restore names exactly the accounts
	// this test changed, rather than re-enabling something another test disabled
	// on purpose.
	rows, err := f.pool.Query(f.ctx,
		`UPDATE users SET disabled_at = now()
		 WHERE is_admin AND disabled_at IS NULL AND id <> $1
		 RETURNING id`, adminID)
	if err != nil {
		t.Fatalf("stand down the other admins: %v", err)
	}
	var stoodDown []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		stoodDown = append(stoodDown, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("stand down the other admins: %v", err)
	}
	t.Cleanup(func() {
		if len(stoodDown) == 0 {
			return
		}
		if _, err := f.pool.Exec(context.Background(),
			`UPDATE users SET disabled_at = NULL WHERE id = ANY($1)`, stoodDown); err != nil {
			t.Errorf("restore the other admins: %v", err)
		}
	})

	// The precondition, asserted rather than assumed: if this is not 1 the test
	// below would pass for the wrong reason.
	var enabled int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM users WHERE is_admin AND disabled_at IS NULL`).Scan(&enabled); err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if enabled != 1 {
		t.Fatalf("%d enabled admins, want exactly 1 — the guard would not fire", enabled)
	}

	path := "/api/v1/admin/users/" + adminID.String()
	for _, c := range []struct {
		what   string
		method string
		body   map[string]any
	}{
		{"demote", http.MethodPatch, map[string]any{"is_admin": false}},
		{"disable", http.MethodPatch, map[string]any{"disabled": true}},
		// DELETE is the same refusal by a different door: it disables.
		{"delete", http.MethodDelete, nil},
	} {
		var body io.Reader
		if c.body != nil {
			body = jsonBody(t, c.body)
		}
		rec := f.do(c.method, path, body, f.admin)
		// 409, not 403: the caller may do this in general, and the server is
		// refusing this instance of it because of the current state.
		if rec.Code != http.StatusConflict {
			t.Errorf("%s the last admin = %d, want 409: %s", c.what, rec.Code, rec.Body)
			continue
		}
		if code := decode(t, rec)["error"].(map[string]any)["code"]; code != "last_admin" {
			t.Errorf("%s the last admin returned code %v, want last_admin", c.what, code)
		}
	}

	// And the refusal happened BEFORE the write: a guard that answers 409 after
	// demoting the account has locked the operator out anyway.
	var isAdmin bool
	var disabledAt *time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT is_admin, disabled_at FROM users WHERE id = $1`, adminID).Scan(&isAdmin, &disabledAt); err != nil {
		t.Fatalf("re-read the admin: %v", err)
	}
	if !isAdmin || disabledAt != nil {
		t.Errorf("the last admin was changed despite the refusal: is_admin=%v disabled_at=%v", isAdmin, disabledAt)
	}

	// The account still works, which is the whole point of the guard.
	if rec := f.do(http.MethodGet, "/api/v1/admin/users", nil, f.admin); rec.Code != http.StatusOK {
		t.Errorf("the last admin lost the admin plane: %d", rec.Code)
	}
}
