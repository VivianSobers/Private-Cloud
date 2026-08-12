package httpapi_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// bearer runs a request carrying a device token in the Authorization header,
// the way the sync client authenticates every call.
func (f *apiFixture) bearer(method, path, token string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// The whole sync-client entry path: an app password minted through the API is
// exchanged for a device bearer token, which then reaches the file plane but not
// credential management.
func TestDeviceTokenExchange(t *testing.T) {
	f := newAPIFixture(t)

	rec := f.json(http.MethodPost, "/api/v1/auth/app-passwords", map[string]any{"name": "laptop"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create app password = %d: %s", rec.Code, rec.Body)
	}
	appPassword := decode(t, rec)["password"].(string)

	// Exchange the app password for a device token.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", nil)
	req.Header.Set("Authorization", basicAuth(f.username, appPassword))
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token exchange = %d: %s", rec.Code, rec.Body)
	}
	token, ok := decode(t, rec)["token"].(string)
	if !ok || token == "" {
		t.Fatalf("no token in exchange response: %s", rec.Body)
	}

	// The token reaches the file plane.
	if rec := f.bearer(http.MethodGet, "/api/v1/nodes/root", token); rec.Code != http.StatusOK {
		t.Errorf("device token on /nodes/root = %d, want 200", rec.Code)
	}
	// And the change journal — the sync client's cursor.
	if rec := f.bearer(http.MethodGet, "/api/v1/changes", token); rec.Code != http.StatusOK {
		t.Errorf("device token on /changes = %d, want 200", rec.Code)
	}
	// And identifying itself.
	if rec := f.bearer(http.MethodGet, "/api/v1/auth/me", token); rec.Code != http.StatusOK {
		t.Errorf("device token on /auth/me = %d, want 200", rec.Code)
	}

	// But it must NOT be able to mint another credential — the app password it
	// came from cannot, and neither may it.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/app-passwords", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("device token minting an app password = %d, want 403", rec.Code)
	}
	// Nor list app passwords or manage sessions.
	if rec := f.bearer(http.MethodGet, "/api/v1/auth/app-passwords", token); rec.Code != http.StatusForbidden {
		t.Errorf("device token listing app passwords = %d, want 403", rec.Code)
	}
	if rec := f.bearer(http.MethodGet, "/api/v1/auth/sessions", token); rec.Code != http.StatusForbidden {
		t.Errorf("device token listing sessions = %d, want 403", rec.Code)
	}
}

// adminDeviceToken mints an app password on the ADMIN account and exchanges it,
// which is the credential the confinement below has to hold against. The
// ordinary deviceToken helper uses the fixture's non-admin user, so it cannot
// tell an authorisation failure apart from a confinement one.
func (f *apiFixture) adminDeviceToken() string {
	f.t.Helper()

	rec := f.do(http.MethodPost, "/api/v1/auth/app-passwords",
		jsonBody(f.t, map[string]any{"name": "admin-laptop"}), f.admin)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create admin app password = %d: %s", rec.Code, rec.Body)
	}
	appPassword := decode(f.t, rec)["password"].(string)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", nil)
	req.Header.Set("Authorization", basicAuth(f.adminUsername, appPassword))
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("admin token exchange = %d: %s", rec.Code, rec.Body)
	}
	return decode(f.t, rec)["token"].(string)
}

// TestDeviceSessionCannotAdminister is the admin-plane half of the confinement.
//
// requireAdmin asks whether the USER is an admin and says nothing about how they
// authenticated, so before this the answer for an app password was "yes" —
// and that credential lives in plaintext in a config file on every synced
// laptop. The cookie session is checked alongside each case so a failure here
// reads as confinement rather than as the fixture's admin not being one.
func TestDeviceSessionCannotAdminister(t *testing.T) {
	f := newAPIFixture(t)
	token := f.adminDeviceToken()

	// It still reaches everything a sync client needs.
	if rec := f.bearer(http.MethodGet, "/api/v1/nodes/root", token); rec.Code != http.StatusOK {
		t.Fatalf("admin device token on /nodes/root = %d, want 200", rec.Code)
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/users"},
		{http.MethodPost, "/api/v1/admin/users"},
		{http.MethodGet, "/api/v1/admin/audit"},
		{http.MethodGet, "/api/v1/admin/storage"},
		{http.MethodPost, "/api/v1/admin/fsck"},
	} {
		if rec := f.bearer(tc.method, tc.path, token); rec.Code != http.StatusForbidden {
			t.Errorf("device token on %s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
		// The same route over the admin's COOKIE session is reachable, which is
		// what proves the 403 above is about the credential and not the account.
		if rec := f.do(tc.method, tc.path, http.NoBody, f.admin); rec.Code == http.StatusForbidden {
			t.Errorf("admin cookie session on %s %s = 403; the fixture admin is not an admin",
				tc.method, tc.path)
		}
	}
}

func TestDeviceTokenRejectsWrongPassword(t *testing.T) {
	f := newAPIFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", nil)
	req.Header.Set("Authorization", basicAuth(f.username, "pcap_0011223344556677_00112233445566778899aabbccddeeff"))
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("exchange with a wrong app password = %d, want 401", rec.Code)
	}

	// No credentials at all is also 401, with a challenge.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", nil)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("exchange with no credentials = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate challenge on unauthenticated exchange")
	}
}
