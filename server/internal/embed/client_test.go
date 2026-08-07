package embed

import (
	"context"
	"encoding/json"
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

func TestClientHealthy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()

	c := NewClient(up.URL, "test", 2)
	if !c.Healthy(context.Background()) {
		t.Error("healthy sidecar reported unhealthy")
	}
	down := NewClient("http://127.0.0.1:1", "test", 2)
	if down.Healthy(context.Background()) {
		t.Error("unreachable sidecar reported healthy")
	}
}
