package httpapi_test

import (
	"net/http"
	"testing"
)

// With no provider configured, the OIDC endpoints report the feature disabled and
// the status endpoint advertises it as off — the passkey paths are unaffected.
func TestOIDCDisabledByDefault(t *testing.T) {
	f := newAPIFixture(t)

	for _, path := range []string{"/api/v1/auth/oidc/login", "/api/v1/auth/oidc/callback"} {
		if rec := f.do(http.MethodGet, path, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s with no provider = %d, want 404", path, rec.Code)
		}
	}

	rec := f.do(http.MethodGet, "/api/v1/auth/status", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if enabled, ok := decode(t, rec)["oidc_enabled"].(bool); !ok || enabled {
		t.Errorf("oidc_enabled = %v, want false", decode(t, rec)["oidc_enabled"])
	}
}
