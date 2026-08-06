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
