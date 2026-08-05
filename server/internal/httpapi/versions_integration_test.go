package httpapi_test

import (
	"net/http"
	"testing"
)

// End-to-end version history over HTTP: the store and service internals are
// covered in the files package; this pins the routing, the JSON shape, and that
// restore and version-download reach the right node.
func TestVersionHistoryOverHTTP(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	// Two writes to the same name build a two-entry history.
	f.upload(root, "notes.txt", "first")
	id := nodeID(t, f.upload(root, "notes.txt", "second"))

	// List: newest first, head flagged, version ids present.
	rec := f.do(http.MethodGet, "/api/v1/nodes/"+id+"/versions", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list versions = %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	versions, ok := body["versions"].([]any)
	if !ok || len(versions) != 2 {
		t.Fatalf("want 2 versions in the history, got: %s", rec.Body)
	}
	newest := versions[0].(map[string]any)
	oldest := versions[1].(map[string]any)
	if newest["is_head"] != true {
		t.Error("newest version is not flagged as head")
	}
	if oldest["is_head"] == true {
		t.Error("the older version is wrongly flagged as head")
	}
	oldVersionID := oldest["id"].(string)

	// Download the OLD version directly: it must serve the first bytes, not the head.
	rec = f.do(http.MethodGet, "/api/v1/nodes/"+id+"/versions/"+oldVersionID+"/content", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("download old version = %d: %s", rec.Code, rec.Body)
	}
	if rec.Body.String() != "first" {
		t.Errorf("old version content = %q, want %q", rec.Body.String(), "first")
	}

	// Restore it: the head now serves the old content, and history grew to three.
	rec = f.json(http.MethodPost, "/api/v1/nodes/"+id+"/versions/"+oldVersionID+"/restore", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore version = %d: %s", rec.Code, rec.Body)
	}

	rec = f.do(http.MethodGet, "/api/v1/nodes/"+id+"/content", nil, f.cookie)
	if rec.Body.String() != "first" {
		t.Errorf("head after restore = %q, want %q", rec.Body.String(), "first")
	}

	rec = f.do(http.MethodGet, "/api/v1/nodes/"+id+"/versions", nil, f.cookie)
	if v := decode(t, rec)["versions"].([]any); len(v) != 3 {
		t.Errorf("history after restore has %d versions, want 3", len(v))
	}
}

// A version id from another file must not be restorable onto this one.
func TestRestoreForeignVersionRejectedOverHTTP(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	aID := nodeID(t, f.upload(root, "a.txt", "aaa"))
	bID := nodeID(t, f.upload(root, "b.txt", "bbb"))

	rec := f.do(http.MethodGet, "/api/v1/nodes/"+bID+"/versions", nil, f.cookie)
	bVersion := decode(t, rec)["versions"].([]any)[0].(map[string]any)["id"].(string)

	rec = f.json(http.MethodPost, "/api/v1/nodes/"+aID+"/versions/"+bVersion+"/restore", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("restoring another file's version = %d, want 404", rec.Code)
	}
}
