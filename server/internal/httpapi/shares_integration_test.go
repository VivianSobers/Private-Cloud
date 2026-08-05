package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// The share plane end to end over HTTP: a link created through the authenticated
// API is then reachable with NO session, gated by a password cookie, and dead
// the instant it is revoked. The service internals are covered in the shares
// package; this pins routing, the public/no-auth boundary, and the cookie.
func TestPublicShareFlowOverHTTP(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	id := nodeID(t, f.upload(root, "public.txt", "hello world"))

	// Create a plain link (authenticated).
	rec := f.json(http.MethodPost, "/api/v1/nodes/"+id+"/shares", map[string]any{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create share = %d: %s", rec.Code, rec.Body)
	}
	share := decode(t, rec)["share"].(map[string]any)
	token := share["token"].(string)
	if token == "" {
		t.Fatal("create did not return a token")
	}

	// View it with NO session cookie — this is the whole point of a share.
	rec = f.do(http.MethodGet, "/api/v1/s/"+token, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public view = %d: %s", rec.Code, rec.Body)
	}
	view := decode(t, rec)
	if view["unlocked"] != true || view["name"] != "public.txt" {
		t.Errorf("unexpected public view: %s", rec.Body)
	}

	// Download it with no session either.
	rec = f.do(http.MethodGet, "/api/v1/s/"+token+"/content", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello world" {
		t.Errorf("public download = %d %q", rec.Code, rec.Body.String())
	}

	// The owner sees it listed; revoking kills the public link immediately.
	rec = f.do(http.MethodGet, "/api/v1/shares", nil, f.cookie)
	shareID := decode(t, rec)["shares"].([]any)[0].(map[string]any)["id"].(string)
	rec = f.do(http.MethodDelete, "/api/v1/shares/"+shareID, nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodGet, "/api/v1/s/"+token, nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("view after revoke = %d, want 404", rec.Code)
	}
}

func TestPasswordShareOverHTTP(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	id := nodeID(t, f.upload(root, "locked.txt", "classified"))

	rec := f.json(http.MethodPost, "/api/v1/nodes/"+id+"/shares", map[string]any{"password": "swordfish"})
	token := decode(t, rec)["share"].(map[string]any)["token"].(string)

	// A locked view reveals only that a password is needed.
	rec = f.do(http.MethodGet, "/api/v1/s/"+token, nil, nil)
	view := decode(t, rec)
	if view["has_password"] != true || view["unlocked"] == true || view["name"] != nil {
		t.Errorf("locked view leaked: %s", rec.Body)
	}

	// Content without the cookie is refused.
	rec = f.do(http.MethodGet, "/api/v1/s/"+token+"/content", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("content before unlock = %d, want 401", rec.Code)
	}

	// Unlock issues a cookie; the content request carrying it succeeds.
	rec = f.do(http.MethodPost, "/api/v1/s/"+token+"/unlock",
		strings.NewReader(`{"password":"swordfish"}`), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlock = %d: %s", rec.Code, rec.Body)
	}
	var unlock *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "pc_share" {
			unlock = c
		}
	}
	if unlock == nil {
		t.Fatal("unlock set no cookie")
	}

	rec = f.do(http.MethodGet, "/api/v1/s/"+token+"/content", nil, unlock)
	if rec.Code != http.StatusOK || rec.Body.String() != "classified" {
		t.Errorf("content after unlock = %d %q", rec.Code, rec.Body.String())
	}
}
