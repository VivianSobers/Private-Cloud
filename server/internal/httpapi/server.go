// Package httpapi wires HTTP routing, middleware and handlers.
//
// Routing uses the standard library's ServeMux. Since Go 1.22 it supports
// method-and-wildcard patterns ("GET /api/v1/nodes/{id}"), which covers what
// this project needs from a router — so there is no third-party router in the
// dependency graph, and none in the path that will later handle auth.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/metrics"
)

type Server struct {
	log     *slog.Logger
	db      *db.DB
	metrics *metrics.Metrics
	version string
	commit  string
	started time.Time
}

func NewServer(log *slog.Logger, database *db.DB, m *metrics.Metrics, version, commit string) *Server {
	return &Server{
		log:     log,
		db:      database,
		metrics: m,
		version: version,
		commit:  commit,
		started: time.Now(),
	}
}

// Handler builds the routing tree wrapped in the middleware chain.
//
// Order is outermost-first and deliberate:
//
//	requestID -> recovery -> observability -> mux
//
// recovery sits outside observability so a panic still produces a metric and a
// log line for the request that caused it; it sits inside requestID so the
// panic log carries the correlation ID.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Operational endpoints, deliberately unauthenticated and unversioned.
	// They are reachable only from the tailnet (Caddy binds to the Tailscale
	// IP), and Kubernetes-style probes must not require credentials.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(
		s.metrics.Registry,
		promhttp.HandlerOpts{
			ErrorHandling:     promhttp.ContinueOnError,
			EnableOpenMetrics: true,
		},
	))

	// The versioned API surface. Slice 1 ships only the identity of the
	// service; nodes, uploads and auth arrive in slices 2 and 3.
	mux.HandleFunc("GET /api/v1/version", s.handleVersion)

	// Anything else under /api/ gets a JSON 404 rather than net/http's plain
	// text, so clients can parse every error the same way.
	mux.HandleFunc("/api/", s.handleNotFound)

	return withRequestID(s.withRecovery(s.withObservability(mux)))
}

// --- handlers ---------------------------------------------------------------

// handleHealthz reports process liveness ONLY. It must not touch the database:
// a liveness probe that fails when a dependency is down causes the orchestrator
// to restart a perfectly healthy process, turning a database blip into a
// crash loop. Dependency health is /readyz.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status":         "ok",
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
	})
}

// handleReadyz reports whether this instance can serve real traffic, which
// means the database must answer.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.db.Ping(ctx); err != nil {
		s.log.Warn("readiness check failed", "error", err, "request_id", RequestID(r.Context()))
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
			"checks": map[string]string{"database": "unreachable"},
		})
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "ready",
		"checks": map[string]string{"database": "ok"},
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]any{
		"service": "private-cloud-api",
		"version": s.version,
		"commit":  s.commit,
		"phase":   "1",
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, "not_found", "no such endpoint")
}

// --- responses --------------------------------------------------------------

// errorBody is the single error shape for the whole API. Defining it once, in
// slice 1, is what stops each later slice from inventing its own.
type errorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// These responses reflect live state; a cached /readyz is worse than none.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so the response cannot be corrected.
		// Log it and move on — the client will see a truncated body.
		slog.Error("encode response", "error", err, "request_id", RequestID(r.Context()))
	}
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	body.Error.RequestID = RequestID(r.Context())
	writeJSON(w, r, status, body)
}
