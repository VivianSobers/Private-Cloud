package httpapi_test

import (
	"net/http"
	"testing"
)

// Without an embedding sidecar configured, semantic search reports itself
// unavailable rather than erroring, and lexical search is untouched — the feature
// is strictly additive.
func TestSemanticSearchUnavailableWithoutEmbedder(t *testing.T) {
	f := newAPIFixture(t)

	rec := f.do(http.MethodGet, "/api/v1/search?q=kittens&semantic=true", nil, f.cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("semantic search without a sidecar = %d, want 503", rec.Code)
	}

	// Lexical search on the same endpoint is unaffected.
	if rec := f.do(http.MethodGet, "/api/v1/search?q=kittens", nil, f.cookie); rec.Code != http.StatusOK {
		t.Errorf("lexical search = %d, want 200", rec.Code)
	}
}
