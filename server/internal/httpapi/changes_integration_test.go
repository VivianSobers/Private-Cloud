package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// The changes cursor endpoint end to end: an upload shows up as an upsert with
// the node embedded, a caught-up client gets nothing, and a client whose cursor
// was pruned out from under it is told to re-sync.
func TestChangesCursorOverHTTP(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	id := nodeID(t, f.upload(root, "sync.txt", "data"))

	rec := f.do(http.MethodGet, "/api/v1/changes?since=0", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("changes = %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)

	// The upload is an upsert, and the node's current state is embedded.
	changes := body["changes"].([]any)
	var embedded map[string]any
	for _, ce := range changes {
		m := ce.(map[string]any)
		if m["node_id"] == id && m["kind"] == "upsert" {
			if node, ok := m["node"].(map[string]any); ok {
				embedded = node
			}
		}
	}
	if embedded == nil || embedded["name"] != "sync.txt" {
		t.Fatalf("upload not reflected as an upsert with node state: %s", rec.Body)
	}

	// A client at the head cursor is caught up: no changes, not a reset.
	latest := int64(body["latest"].(float64))
	rec = f.do(http.MethodGet, fmt.Sprintf("/api/v1/changes?since=%d", latest), nil, f.cookie)
	caught := decode(t, rec)
	if n := len(caught["changes"].([]any)); n != 0 {
		t.Errorf("caught-up client received %d changes", n)
	}
	if caught["reset"] != false {
		t.Error("caught-up client was told to reset")
	}
}

func TestChangesResetAfterPrune(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	f.upload(root, "a.txt", "1")
	f.upload(root, "b.txt", "2")

	// Simulate retention having pruned this owner's journal tail while the counter
	// (sync_state) marches on — a client resuming from an old cursor must reset.
	if _, err := f.pool.Exec(context.Background(),
		`DELETE FROM changes WHERE owner_id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}

	rec := f.do(http.MethodGet, "/api/v1/changes?since=1", nil, f.cookie)
	body := decode(t, rec)
	if body["reset"] != true {
		t.Errorf("a cursor behind the pruned journal was not told to reset: %s", rec.Body)
	}
}
