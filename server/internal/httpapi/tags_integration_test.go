package httpapi_test

import (
	"net/http"
	"testing"
)

// The tag API round-trip: add a user tag to a file, see it on the node and in the
// tag list, filter files by it, then remove it.
func TestTagAPILifecycle(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	rec := f.upload(root, "report.txt", "quarterly numbers")
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body)
	}
	id := nodeID(t, rec)

	// Add a user tag.
	rec = f.json(http.MethodPost, "/api/v1/nodes/"+id+"/tags", map[string]any{"tag": "Q3"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add tag = %d: %s", rec.Code, rec.Body)
	}

	// It appears on the node's tag list.
	rec = f.do(http.MethodGet, "/api/v1/nodes/"+id+"/tags", nil, f.cookie)
	tags := decode(t, rec)["tags"].([]any)
	if !hasTag(tags, "q3") {
		t.Errorf("tag not listed on node: %v", tags)
	}

	// And in the node's own GET.
	rec = f.do(http.MethodGet, "/api/v1/nodes/"+id, nil, f.cookie)
	if nt, ok := decode(t, rec)["tags"].([]any); !ok || !hasTag(nt, "q3") {
		t.Errorf("tag not on node GET: %v", decode(t, rec)["tags"])
	}

	// Filtering by the tag returns the file.
	rec = f.do(http.MethodGet, "/api/v1/tags/q3", nil, f.cookie)
	if n := decode(t, rec)["count"].(float64); n != 1 {
		t.Errorf("tag filter count = %v, want 1", n)
	}

	// Remove it.
	rec = f.do(http.MethodDelete, "/api/v1/nodes/"+id+"/tags/q3", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove tag = %d: %s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodGet, "/api/v1/nodes/"+id+"/tags", nil, f.cookie)
	if hasTag(decode(t, rec)["tags"].([]any), "q3") {
		t.Error("tag survived removal")
	}
}

func hasTag(tags []any, name string) bool {
	for _, t := range tags {
		if m, ok := t.(map[string]any); ok && m["name"] == name {
			return true
		}
	}
	return false
}
