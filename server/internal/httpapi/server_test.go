package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/metrics"
)

// newTestServer builds a Server with no database, auth service or file service.
//
// Handlers that touch any of them (/readyz, /api/v1/auth/*, /api/v1/nodes/*)
// need a real Postgres and are covered by integration tests; these tests cover
// routing, middleware and response shape, which need none of it.
func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New("test", "abc123", func() float64 { return 0 })
	return NewServer(log, nil, m, nil, nil, Options{Version: "test", Commit: "abc123"}).Handler()
}

func TestHealthzIsOKWithoutDatabase(t *testing.T) {
	// The point of liveness: it must succeed even with no database at all.
	// If this ever starts requiring the DB, a database blip becomes a crash
	// loop in production.
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want ok", body["status"])
	}
}

func TestVersionEndpoint(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["version"] != "test" || body["commit"] != "abc123" {
		t.Errorf("build info not propagated: %v", body)
	}
}

func TestUnknownAPIPathReturnsJSONError(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}

	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("404 body is not the standard error shape: %v", err)
	}
	if body.Error.Code != "not_found" {
		t.Errorf("error code = %q, want not_found", body.Error.Code)
	}
	if body.Error.RequestID == "" {
		t.Error("error body should carry the request ID for correlation")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	// ServeMux with method patterns returns 405 for a known path with the
	// wrong method. Asserting it here documents the behaviour we rely on.
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("X-Request-Id header missing from response")
	}
}

func TestInboundRequestIDIsPreserved(t *testing.T) {
	// Correlation must survive the hop through Caddy.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "caddy-generated-id")

	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != "caddy-generated-id" {
		t.Errorf("X-Request-Id = %q, want the inbound value", got)
	}
}

func TestOverlongRequestIDIsReplaced(t *testing.T) {
	// An attacker-controlled header ends up in every log line for the request;
	// unbounded length would let one request write arbitrarily large logs.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", strings.Repeat("A", 5000))

	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); len(got) > 64 {
		t.Errorf("overlong inbound request ID was not replaced (len %d)", len(got))
	}
}

func TestMetricsEndpointExposesOurSeries(t *testing.T) {
	h := newTestServer(t)

	// Generate one request so the HTTP counters exist.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"privatecloud_http_requests_total",
		"privatecloud_build_info",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

// Route labels must be the ServeMux pattern, not the concrete path — otherwise
// every distinct URL becomes its own time series. This is the single most
// expensive mistake available in Prometheus instrumentation.
func TestMetricsLabelByRoutePatternNotPath(t *testing.T) {
	h := newTestServer(t)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `route="GET /api/v1/version"`) {
		t.Errorf("expected a route label holding the mux pattern; got:\n%s", body)
	}
}

// Unmatched paths must collapse to a single label, or a port scanner probing
// random URLs becomes a cardinality attack on Prometheus.
func TestUnmatchedRoutesShareOneLabel(t *testing.T) {
	h := newTestServer(t)
	for _, p := range []string{"/api/aaa", "/api/bbb", "/api/ccc"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	for _, p := range []string{`route="/api/aaa"`, `route="/api/bbb"`} {
		if strings.Contains(body, p) {
			t.Errorf("raw path leaked into a metric label: %s", p)
		}
	}
}

func TestPanicIsRecovered(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New("test", "abc", func() float64 { return 0 })
	s := NewServer(log, nil, m, nil, nil, Options{Version: "test", Commit: "abc"})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("deliberate test panic")
	})
	h := withRequestID(s.withRecovery(s.withObservability(mux)))

	rec := httptest.NewRecorder()
	// If recovery is broken this panics out of ServeHTTP and fails the test,
	// which is exactly the signal we want.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("panic response is not the standard error shape: %v", err)
	}
	if body.Error.Code != "internal_error" {
		t.Errorf("error code = %q, want internal_error", body.Error.Code)
	}
}

func TestStatusClass(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{200, "2xx"}, {201, "2xx"}, {301, "3xx"},
		{404, "4xx"}, {422, "4xx"}, {500, "5xx"}, {503, "5xx"},
	} {
		if got := statusClass(tc.code); got != tc.want {
			t.Errorf("statusClass(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}
