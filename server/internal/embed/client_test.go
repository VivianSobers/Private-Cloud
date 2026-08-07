package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req embedRequest
		json.NewDecoder(r.Body).Decode(&req)
		// One 2-dim vector per text.
		vecs := make([][]float32, len(req.Texts))
		for i := range req.Texts {
			vecs[i] = []float32{float32(i), float32(len(req.Texts[i]))}
		}
		json.NewEncoder(w).Encode(embedResponse{Model: "test", Dim: 2, Vectors: vecs})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test", 2)
	vecs, err := c.Embed(context.Background(), []string{"aa", "bbbb"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 2 || vecs[1][1] != 4 {
		t.Errorf("unexpected vectors: %v", vecs)
	}
	if c.Model() != "test" || c.Dim() != 2 {
		t.Errorf("model/dim wrong: %s/%d", c.Model(), c.Dim())
	}
}

// A sidecar returning the wrong shape is caught at the client, not passed on as
// corrupt vectors.
func TestClientRejectsBadShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One vector for two texts, and the wrong dimension.
		json.NewEncoder(w).Encode(embedResponse{Model: "test", Dim: 2, Vectors: [][]float32{{1, 2, 3}}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test", 2)
	if _, err := c.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Error("expected an error for a mismatched vector count")
	}
}

// healthzServer serves a /healthz reporting the given model and dimension.
func healthzServer(t *testing.T, model string, dim int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","model":%q,"dim":%d,"device":"cpu"}`, model, dim)
	}))
}

func TestClientVerify(t *testing.T) {
	up := healthzServer(t, "BAAI/bge-small-en-v1.5", 2)
	defer up.Close()

	c := NewClient(up.URL, "bge-small-en-v1.5", 2)
	info, err := c.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if info.Dim != 2 || info.Model != "BAAI/bge-small-en-v1.5" {
		t.Errorf("Verify returned %+v", info)
	}
	// The stored identity drops the vendor prefix; that is the same model.
	if !c.SameModel(info.Model) {
		t.Error("SameModel must ignore a vendor prefix and case")
	}

	// A sidecar swapped to another model of the same width is exactly the case
	// nothing else in the system can detect.
	if c.SameModel("all-MiniLM-L6-v2") {
		t.Error("SameModel must not treat a different model as a match")
	}
}

func TestClientVerifyDimMismatch(t *testing.T) {
	up := healthzServer(t, "bge-small-en-v1.5", 384)
	defer up.Close()

	c := NewClient(up.URL, "bge-small-en-v1.5", 2)
	if _, err := c.Verify(context.Background()); !errors.Is(err, ErrDimMismatch) {
		t.Fatalf("Verify error = %v, want ErrDimMismatch", err)
	}
}

func TestClientVerifyUnreachable(t *testing.T) {
	down := NewClient("http://127.0.0.1:1", "test", 2)
	err := errFrom(down.Verify(context.Background()))
	if err == nil {
		t.Fatal("expected an error for an unreachable sidecar")
	}
	// Transport failure must NOT look like a mismatch: the caller keeps the
	// feature enabled for one and disables it for the other.
	if errors.Is(err, ErrDimMismatch) {
		t.Error("an unreachable sidecar must not report a dimension mismatch")
	}
}

func errFrom(_ SidecarInfo, err error) error { return err }
