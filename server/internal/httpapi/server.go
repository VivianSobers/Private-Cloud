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

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/metrics"
)

type Server struct {
	log     *slog.Logger
	db      *db.DB
	metrics *metrics.Metrics
	auth    *auth.Service
	files   *files.Service
	version string
	commit  string
	started time.Time

	cookieName   string
	cookieSecure bool

	// Guards the auth endpoints against online guessing. 20/min with a burst
	// of 10 is invisible to a human signing in and useless to a script.
	authLimiter *rateLimiter
}

type Options struct {
	Version      string
	Commit       string
	CookieName   string
	CookieSecure bool
}

func NewServer(log *slog.Logger, database *db.DB, m *metrics.Metrics, authSvc *auth.Service, filesSvc *files.Service, opts Options) *Server {
	if opts.CookieName == "" {
		opts.CookieName = "pc_session"
	}
	return &Server{
		log:          log,
		db:           database,
		metrics:      m,
		auth:         authSvc,
		files:        filesSvc,
		version:      opts.Version,
		commit:       opts.Commit,
		started:      time.Now(),
		cookieName:   opts.CookieName,
		cookieSecure: opts.CookieSecure,
		authLimiter:  newRateLimiter(20, 10),
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

	mux.HandleFunc("GET /api/v1/version", s.handleVersion)

	// --- auth: unauthenticated, rate limited --------------------------------
	// rateLimited wraps each individually rather than the group, because these
	// live on the same mux as everything else and a path-prefix check would
	// have to be kept in sync by hand.
	rl := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.withRateLimit(h).ServeHTTP(w, r)
		}
	}

	mux.HandleFunc("GET /api/v1/auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /api/v1/auth/register/begin", rl(s.handleRegisterBegin))
	mux.HandleFunc("POST /api/v1/auth/register/finish", rl(s.handleRegisterFinish))
	mux.HandleFunc("POST /api/v1/auth/login/begin", rl(s.handleLoginBegin))
	mux.HandleFunc("POST /api/v1/auth/login/finish", rl(s.handleLoginFinish))
	mux.HandleFunc("POST /api/v1/auth/recovery/redeem", rl(s.handleRecoveryRedeem))
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)

	// --- auth: authenticated -------------------------------------------------
	mux.HandleFunc("GET /api/v1/auth/me", s.requireAuth(s.handleMe))
	mux.HandleFunc("GET /api/v1/auth/credentials", s.requireAuth(s.handleListCredentials))
	mux.HandleFunc("DELETE /api/v1/auth/credentials/{id}", s.requireAuth(s.handleDeleteCredential))
	mux.HandleFunc("GET /api/v1/auth/sessions", s.requireAuth(s.handleListSessions))
	mux.HandleFunc("DELETE /api/v1/auth/sessions/{id}", s.requireAuth(s.handleRevokeSession))
	mux.HandleFunc("POST /api/v1/auth/recovery/regenerate", s.requireAuth(s.handleRegenerateRecoveryCodes))

	// --- files ---------------------------------------------------------------
	// Every route below is requireAuth-wrapped individually. A prefix-matched
	// "protect everything under /api/v1/nodes" rule would silently stop
	// protecting the day someone adds a route one path segment to the side.
	mux.HandleFunc("GET /api/v1/nodes/root", s.requireAuth(s.handleGetRoot))
	mux.HandleFunc("GET /api/v1/nodes/resolve", s.requireAuth(s.handleResolvePath))
	mux.HandleFunc("GET /api/v1/nodes/{id}", s.requireAuth(s.handleGetNode))
	mux.HandleFunc("GET /api/v1/nodes/{id}/children", s.requireAuth(s.handleListChildren))
	mux.HandleFunc("PATCH /api/v1/nodes/{id}", s.requireAuth(s.handlePatchNode))
	mux.HandleFunc("DELETE /api/v1/nodes/{id}", s.requireAuth(s.handleTrashNode))
	mux.HandleFunc("POST /api/v1/folders", s.requireAuth(s.handleCreateFolder))

	// HEAD is registered explicitly. ServeMux does not imply it from GET, and
	// without it a client checking a file's size before downloading gets a 405.
	mux.HandleFunc("GET /api/v1/nodes/{id}/content", s.requireAuth(s.handleDownload))
	mux.HandleFunc("HEAD /api/v1/nodes/{id}/content", s.requireAuth(s.handleDownload))
	mux.HandleFunc("POST /api/v1/upload", s.requireAuth(s.handleUpload))

	// --- resumable uploads (tus 1.0.0) ---------------------------------------
	// OPTIONS is unauthenticated: it advertises protocol support and nothing
	// else, and tus clients send it before they have anywhere to put
	// credentials.
	mux.HandleFunc("OPTIONS /api/v1/uploads", s.handleTusOptions)
	mux.HandleFunc("POST /api/v1/uploads", s.requireAuth(s.handleTusCreate))
	mux.HandleFunc("HEAD /api/v1/uploads/{id}", s.requireAuth(s.handleTusHead))
	mux.HandleFunc("PATCH /api/v1/uploads/{id}", s.requireAuth(s.handleTusPatch))
	mux.HandleFunc("DELETE /api/v1/uploads/{id}", s.requireAuth(s.handleTusDelete))

	mux.HandleFunc("GET /api/v1/trash", s.requireAuth(s.handleListTrash))
	mux.HandleFunc("DELETE /api/v1/trash", s.requireAuth(s.handleEmptyTrash))
	mux.HandleFunc("POST /api/v1/trash/{id}/restore", s.requireAuth(s.handleRestoreNode))
	mux.HandleFunc("DELETE /api/v1/trash/{id}", s.requireAuth(s.handlePurgeNode))

	mux.HandleFunc("GET /api/v1/usage", s.requireAuth(s.handleUsage))

	// App passwords: credentials for clients that cannot run a WebAuthn
	// ceremony. Managed through the session-authenticated API, never through
	// WebDAV itself — an app password must not be able to mint another.
	mux.HandleFunc("GET /api/v1/auth/app-passwords", s.requireAuth(s.handleListAppPasswords))
	mux.HandleFunc("POST /api/v1/auth/app-passwords", s.requireAuth(s.handleCreateAppPassword))
	mux.HandleFunc("DELETE /api/v1/auth/app-passwords/{id}", s.requireAuth(s.handleRevokeAppPassword))

	// fsck walks the entire blob store and can delete orphans; admin only.
	mux.HandleFunc("POST /api/v1/admin/fsck", s.requireAdmin(s.handleFsck))

	// --- WebDAV --------------------------------------------------------------
	// Mounted outside /api because it is a different protocol with a different
	// auth scheme. Registered as a prefix rather than with method patterns:
	// WebDAV uses PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK and UNLOCK,
	// which ServeMux would otherwise reject as unknown methods.
	mux.Handle(davPrefix+"/", s.withDavAuth(s.davHandler()))
	mux.Handle(davPrefix, s.withDavAuth(s.davHandler()))

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
