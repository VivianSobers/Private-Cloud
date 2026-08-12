package httpapi_test

import (
	"net/http"
	"testing"
)

// Sharing over HTTP.
//
// The single most important test in this file is the compatibility one: without
// ?include_shared=true, every endpoint must return exactly what it returned
// before Phase 7. A client written against the old behaviour assumed everything
// it saw was its own, and quietly widening the default would change what its
// result list MEANS without changing its shape — the worst kind of break,
// because nothing errors.

func TestGrantAndReadSharedFile(t *testing.T) {
	f := newAPIFixture(t)
	id := nodeID(t, f.upload(f.root(), "shared.txt", "hello"))

	rec := f.json(http.MethodPost, "/api/v1/nodes/"+id+"/grants",
		map[string]any{"username": f.adminUsername, "role": "viewer"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create grant = %d: %s", rec.Code, rec.Body)
	}
	grant := decode(t, rec)["grant"].(map[string]any)
	if grant["role"] != "viewer" || grant["grantee"] != f.adminUsername {
		t.Fatalf("unexpected grant: %v", grant)
	}

	// The grantee sees it in /shared, with an access object explaining why.
	rec = f.do(http.MethodGet, "/api/v1/shared", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /shared = %d: %s", rec.Code, rec.Body)
	}
	items := decode(t, rec)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("got %d shared item(s), want 1", len(items))
	}
	access := items[0].(map[string]any)["access"].(map[string]any)
	if access["role"] != "viewer" || access["shared"] != true {
		t.Errorf("access = %v, want a shared viewer role", access)
	}

	// And can read the bytes.
	if rec := f.do(http.MethodGet, "/api/v1/nodes/"+id+"/content", nil, f.admin); rec.Code != http.StatusOK {
		t.Errorf("grantee download = %d, want 200", rec.Code)
	}
}

// THE compatibility guarantee.
func TestSharedContentIsInvisibleWithoutTheOptIn(t *testing.T) {
	f := newAPIFixture(t)
	root := f.root()
	folder := nodeID(t, f.json(http.MethodPost, "/api/v1/folders",
		map[string]any{"parent_id": root, "name": "project"}))
	f.upload(folder, "widgets.txt", "a specification")

	if rec := f.json(http.MethodPost, "/api/v1/nodes/"+folder+"/grants",
		map[string]any{"username": f.adminUsername, "role": "viewer"}); rec.Code != http.StatusCreated {
		t.Fatalf("create grant = %d: %s", rec.Code, rec.Body)
	}

	// The grantee's OWN root listing must not have grown.
	rec := f.do(http.MethodGet, "/api/v1/nodes/root/children", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("children = %d", rec.Code)
	}
	if kids := decode(t, rec)["children"].([]any); len(kids) != 0 {
		t.Errorf("a share appeared in the grantee's own tree: %d children", len(kids))
	}

	// Search without the opt-in must not find it.
	rec = f.do(http.MethodGet, "/api/v1/search?q=widgets", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d: %s", rec.Code, rec.Body)
	}
	if results := decode(t, rec)["results"].([]any); len(results) != 0 {
		t.Errorf("default search returned %d shared result(s) — the default must be unchanged", len(results))
	}

	// With the opt-in, it appears.
	rec = f.do(http.MethodGet, "/api/v1/search?q=widgets&include_shared=true", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("shared search = %d: %s", rec.Code, rec.Body)
	}
	results := decode(t, rec)["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("opt-in search returned %d result(s), want 1", len(results))
	}
	if _, ok := results[0].(map[string]any)["access"]; !ok {
		t.Error("a shared search result carries no access object")
	}
}

// Navigating INTO a shared folder needs the opt-in too, and works with it.
func TestBrowsingIntoASharedFolder(t *testing.T) {
	f := newAPIFixture(t)
	root := f.root()
	folder := nodeID(t, f.json(http.MethodPost, "/api/v1/folders",
		map[string]any{"parent_id": root, "name": "project"}))
	f.upload(folder, "a.txt", "a")

	if rec := f.json(http.MethodPost, "/api/v1/nodes/"+folder+"/grants",
		map[string]any{"username": f.adminUsername, "role": "editor"}); rec.Code != http.StatusCreated {
		t.Fatalf("grant = %d", rec.Code)
	}

	// Without the opt-in the folder is not the caller's, so it is simply absent.
	if rec := f.do(http.MethodGet, "/api/v1/nodes/"+folder+"/children", nil, f.admin); rec.Code != http.StatusNotFound {
		t.Errorf("listing a shared folder without the opt-in = %d, want 404", rec.Code)
	}

	rec := f.do(http.MethodGet, "/api/v1/nodes/"+folder+"/children?include_shared=true", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("opt-in children = %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	kids := body["children"].([]any)
	if len(kids) != 1 {
		t.Fatalf("got %d child(ren), want 1", len(kids))
	}
	if _, ok := kids[0].(map[string]any)["access"]; !ok {
		t.Error("a child of a shared folder carries no access object")
	}
	// The parent carries one too, so a UI can render "editor" on the folder.
	if _, ok := body["parent"].(map[string]any)["access"]; !ok {
		t.Error("the shared parent carries no access object")
	}
}

// A node the caller owns must never carry `access` — its absence is what means
// "mine", and adding it would change the response for every existing client.
func TestOwnNodesCarryNoAccessObject(t *testing.T) {
	f := newAPIFixture(t)
	f.upload(f.root(), "mine.txt", "mine")

	rec := f.do(http.MethodGet, "/api/v1/nodes/root/children?include_shared=true", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("children = %d", rec.Code)
	}
	for _, k := range decode(t, rec)["children"].([]any) {
		if _, ok := k.(map[string]any)["access"]; ok {
			t.Error("a node the caller owns carries an access object")
		}
	}
}

// Granting an unknown username must not confirm or deny that it exists.
func TestGrantToUnknownUserIsANotFound(t *testing.T) {
	f := newAPIFixture(t)
	id := nodeID(t, f.upload(f.root(), "doc.txt", "doc"))

	rec := f.json(http.MethodPost, "/api/v1/nodes/"+id+"/grants",
		map[string]any{"username": "nobody-here-at-all", "role": "viewer"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("grant to an unknown user = %d, want 404", rec.Code)
	}
}

// Only the owner may grant, over HTTP as well as in the store.
func TestGranteeCannotResharOverHTTP(t *testing.T) {
	f := newAPIFixture(t)
	id := nodeID(t, f.upload(f.root(), "doc.txt", "doc"))

	if rec := f.json(http.MethodPost, "/api/v1/nodes/"+id+"/grants",
		map[string]any{"username": f.adminUsername, "role": "editor"}); rec.Code != http.StatusCreated {
		t.Fatalf("grant = %d", rec.Code)
	}

	// The editor tries to grant the same node onward.
	rec := f.do(http.MethodPost, "/api/v1/nodes/"+id+"/grants",
		jsonBody(t, map[string]any{"username": f.username, "role": "viewer"}), f.admin)
	if rec.Code != http.StatusNotFound {
		t.Errorf("an editor re-shared over HTTP = %d, want 404", rec.Code)
	}
}

func TestRevokingAGrantOverHTTP(t *testing.T) {
	f := newAPIFixture(t)
	id := nodeID(t, f.upload(f.root(), "doc.txt", "doc"))

	rec := f.json(http.MethodPost, "/api/v1/nodes/"+id+"/grants",
		map[string]any{"username": f.adminUsername, "role": "viewer"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("grant = %d", rec.Code)
	}
	grantID := decode(t, rec)["grant"].(map[string]any)["id"].(string)

	if rec := f.json(http.MethodDelete, "/api/v1/grants/"+grantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body)
	}
	// Access is gone on the very next request.
	if rec := f.do(http.MethodGet, "/api/v1/nodes/"+id+"/content", nil, f.admin); rec.Code != http.StatusNotFound {
		t.Errorf("grantee can still read after revocation: %d", rec.Code)
	}
}

// Both directions in one call, so a UI need not correlate two lists.
func TestListGrantsReturnsBothDirections(t *testing.T) {
	f := newAPIFixture(t)
	id := nodeID(t, f.upload(f.root(), "doc.txt", "doc"))
	if rec := f.json(http.MethodPost, "/api/v1/nodes/"+id+"/grants",
		map[string]any{"username": f.adminUsername, "role": "viewer"}); rec.Code != http.StatusCreated {
		t.Fatalf("grant = %d", rec.Code)
	}

	body := decode(t, f.json(http.MethodGet, "/api/v1/grants", nil))
	if len(body["granted"].([]any)) != 1 {
		t.Errorf("granter sees %d granted", len(body["granted"].([]any)))
	}
	if len(body["received"].([]any)) != 0 {
		t.Errorf("granter should have received nothing")
	}

	body = decode(t, f.do(http.MethodGet, "/api/v1/grants", nil, f.admin))
	if len(body["received"].([]any)) != 1 {
		t.Errorf("grantee sees %d received", len(body["received"].([]any)))
	}
}

// Tag counts are per caller. A shared count would tell one user how many files
// another has tagged.
func TestTagCountsAreScopedToTheCaller(t *testing.T) {
	f := newAPIFixture(t)
	id := nodeID(t, f.upload(f.root(), "doc.txt", "doc"))
	if rec := f.json(http.MethodPost, "/api/v1/nodes/"+id+"/tags",
		map[string]any{"tag": "receipts"}); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("add tag = %d: %s", rec.Code, rec.Body)
	}

	// The other user has tagged nothing and must see no counts.
	body := decode(t, f.do(http.MethodGet, "/api/v1/tags", nil, f.admin))
	for _, entry := range body["tags"].([]any) {
		if entry.(map[string]any)["tag"] == "receipts" {
			t.Error("one user's tag count is visible to another")
		}
	}
}
