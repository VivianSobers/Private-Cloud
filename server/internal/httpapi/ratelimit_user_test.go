package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/metrics"
)

func newLimiterServer(t *testing.T) *Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New("test", "abc123", func() float64 { return 0 })
	s := NewServer(log, nil, m, nil, nil, nil, Options{Version: "test", Commit: "abc123"})
	t.Cleanup(func() {
		s.authLimiter.close()
		s.searchLimiter.close()
		s.userLimiter.close()
		s.heavyLimiter.close()
	})
	return s
}

// The cost table names routes by their registered pattern, so an entry that
// matches nothing is a line that silently stopped doing anything — the same
// failure mode `awaitingClient` guards against, and the reason both are checked
// rather than trusted.
func TestRouteCostTableHasNoStaleEntries(t *testing.T) {
	registered := map[string]bool{}
	for _, p := range newLimiterServer(t).Routes() {
		registered[p] = true
	}

	for pattern := range routeCosts {
		if !registered[pattern] {
			t.Errorf("routeCosts names %q, which no route registers", pattern)
		}
	}
}

// The two properties that make the classification worth having: the endpoints
// that spend somebody else's GPU are heavy, and the ones that move bytes are not
// metered by request count.
func TestRouteCostClassification(t *testing.T) {
	heavy := []string{
		"POST /api/v1/chat",
		"GET /api/v1/nodes/{id}/similar",
	}
	for _, p := range heavy {
		if routeCosts[p] != costHeavy {
			t.Errorf("%s should be costHeavy: it is an RPC to a shared sidecar", p)
		}
	}

	// A sync pushing a large file issues one request per chunk. Metering that by
	// request count would throttle a first sync while doing nothing about the
	// bytes, which are what actually cost something.
	stream := []string{
		"PUT /api/v1/chunks/{hash}",
		"GET /api/v1/chunks/{hash}",
		"POST /api/v1/manifests",
		"PATCH /api/v1/uploads/{id}",
		"GET /api/v1/nodes/{id}/content",
		"GET /api/v1/changes",
	}
	for _, p := range stream {
		if routeCosts[p] != costStream {
			t.Errorf("%s should be costStream: it is bounded by bytes and quota, not by request count", p)
		}
	}

	// An unclassified route is limited, never exempt. Getting the table wrong
	// must only ever be too strict.
	if routeCosts["GET /api/v1/nodes/{id}/children"] != costNormal {
		t.Error("an unlisted route should default to costNormal")
	}
}

func request(pattern, path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Pattern = pattern
	return r
}

func TestUserLimiterExemptsTheDataPlane(t *testing.T) {
	s := newLimiterServer(t)
	// A bucket of one, so anything counted is refused on the second request.
	s.userLimiter = newRateLimiter(60, 1)
	s.heavyLimiter = newRateLimiter(60, 1)
	t.Cleanup(func() {
		s.userLimiter.close()
		s.heavyLimiter.close()
	})

	r := request("PUT /api/v1/chunks/{hash}", "/api/v1/chunks/abc")
	for i := 0; i < 50; i++ {
		if !s.userAllowed(httptest.NewRecorder(), r, "user-1") {
			t.Fatalf("chunk upload %d was rate limited; a first sync would never finish", i+1)
		}
	}
}

func TestHeavyRoutesGetTheTighterBudget(t *testing.T) {
	s := newLimiterServer(t)
	s.userLimiter = newRateLimiter(60000, 1000)
	s.heavyLimiter = newRateLimiter(60, 2)
	t.Cleanup(func() {
		s.userLimiter.close()
		s.heavyLimiter.close()
	})

	chat := request("POST /api/v1/chat", "/api/v1/chat")
	for i := 0; i < 2; i++ {
		if !s.userAllowed(httptest.NewRecorder(), chat, "user-1") {
			t.Fatalf("chat request %d within the burst was refused", i+1)
		}
	}

	rec := httptest.NewRecorder()
	if s.userAllowed(rec, chat, "user-1") {
		t.Fatal("a third chat request should have exhausted the heavy budget")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 without Retry-After tells the client nothing about when to come back")
	}

	// The ordinary API is untouched by one user exhausting the heavy budget:
	// asking too many questions must not stop them opening a folder.
	if !s.userAllowed(httptest.NewRecorder(), request("GET /api/v1/nodes/{id}/children", "/api/v1/nodes/x/children"), "user-1") {
		t.Error("the heavy budget should not spend the general one")
	}
}

func TestUserLimitsAreNotShared(t *testing.T) {
	s := newLimiterServer(t)
	s.heavyLimiter = newRateLimiter(60, 1)
	t.Cleanup(s.heavyLimiter.close)

	chat := request("POST /api/v1/chat", "/api/v1/chat")
	if !s.userAllowed(httptest.NewRecorder(), chat, "user-1") {
		t.Fatal("the first request should be allowed")
	}
	if s.userAllowed(httptest.NewRecorder(), chat, "user-1") {
		t.Fatal("the same user should be out of tokens")
	}
	// The whole reason for keying on the user rather than the address: one heavy
	// account is slowed without touching anybody else.
	if !s.userAllowed(httptest.NewRecorder(), chat, "user-2") {
		t.Fatal("a second user must have their own budget")
	}
}
