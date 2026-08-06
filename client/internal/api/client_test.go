package api

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeebo/blake3"
)

// A device token is minted from Basic auth, then every other call carries it as
// a Bearer token — never the app password again.
func TestAuthenticatesThenUsesBearer(t *testing.T) {
	var tokenCalls, rootCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/token":
			atomic.AddInt32(&tokenCalls, 1)
			user, pass, ok := r.BasicAuth()
			if !ok || user != "vivian" || pass != "pcap_secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"token":"tok-123","expires_at":"2999-01-01T00:00:00Z"}`))
		case "/api/v1/nodes/root":
			atomic.AddInt32(&rootCalls, 1)
			if r.Header.Get("Authorization") != "Bearer tok-123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"node":{"id":"root-id","kind":"folder","name":"","path":"/"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "vivian", "pcap_secret", "test")
	root, err := c.GetRoot(context.Background())
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if root.ID != "root-id" || !root.IsFolder() {
		t.Errorf("unexpected root: %+v", root)
	}

	// A second call reuses the cached token — no re-authentication.
	if _, err := c.GetRoot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 1 {
		t.Errorf("authenticated %d times, want 1", tokenCalls)
	}
	if rootCalls < 2 {
		t.Errorf("root called %d times, want >= 2", rootCalls)
	}
}

// An expired token surfaces as a 401; the client re-authenticates once and
// retries, transparently to the caller.
func TestRefreshesOn401(t *testing.T) {
	var issued int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/token":
			atomic.AddInt32(&issued, 1)
			w.Write([]byte(`{"token":"fresh","expires_at":"2999-01-01T00:00:00Z"}`))
		case "/api/v1/nodes/root":
			// Only a freshly-minted token is accepted; the primed "stale" one is not.
			if r.Header.Get("Authorization") != "Bearer fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"node":{"id":"root","kind":"folder"}}`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "u", "p", "test")
	// Prime a stale token so the first root call 401s and forces a refresh. The
	// expiry is far future so the client believes the token is still valid and
	// learns otherwise only from the 401.
	c.token, c.expiry = "stale", time.Now().Add(time.Hour)
	if _, err := c.GetRoot(context.Background()); err != nil {
		t.Fatalf("expected transparent refresh, got %v", err)
	}
	if issued != 1 {
		t.Errorf("token issued %d times; the stale token should have driven exactly one refresh", issued)
	}
}

// A server that serves the wrong bytes under a chunk hash is caught by the same
// verification the server applies to uploads.
func TestGetChunkVerifiesAddress(t *testing.T) {
	honest := []byte("the real chunk bytes")
	sum := blake3.Sum256(honest)
	hashHex := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/token":
			w.Write([]byte(`{"token":"t","expires_at":"2999-01-01T00:00:00Z"}`))
		default: // any chunk path
			w.Header().Set("X-Chunk-Compression", "none")
			w.Header().Set("X-Chunk-Plain-Size", strconv.Itoa(len(honest)))
			w.Write([]byte("tampered bytes!!!!!!")) // same length, wrong content
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "u", "p", "test")
	if _, err := c.GetChunk(context.Background(), hashHex); err == nil {
		t.Error("tampered chunk passed verification")
	}
}
