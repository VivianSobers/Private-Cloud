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
	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/metrics"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/shares"
)

type Server struct {
	log     *slog.Logger
	db      *db.DB
	metrics *metrics.Metrics
	auth    *auth.Service
	files   *files.Service
	shares  *shares.Service
	version string
	commit  string
	started time.Time

	cookieName   string
	cookieSecure bool

	// embedder is set when a semantic-search sidecar is configured; nil otherwise,
	// in which case semantic queries are answered "unavailable" and lexical search
	// is unaffected. It embeds the QUERY only — an RPC to the sidecar, never a
	// resident model in this process.
	embedder embed.Embedder

	// oidc is set when a single sign-on provider is configured; nil otherwise, in
	// which case the OIDC endpoints report the feature disabled and the passkey
	// paths are entirely unaffected.
	oidc *auth.OIDCProvider

	// Guards the auth endpoints against online guessing. 20/min with a burst
	// of 10 is invisible to a human signing in and useless to a script.
	authLimiter *rateLimiter
}

// SetEmbedder enables semantic search by wiring the query embedder. Left unset
// when no inference sidecar is configured.
func (s *Server) SetEmbedder(e embed.Embedder) { s.embedder = e }

type Options struct {
	Version      string
	Commit       string
	CookieName   string
	CookieSecure bool
}

func NewServer(log *slog.Logger, database *db.DB, m *metrics.Metrics, authSvc *auth.Service, filesSvc *files.Service, sharesSvc *shares.Service, opts Options) *Server {
	if opts.CookieName == "" {
		opts.CookieName = "pc_session"
	}
	return &Server{
		log:          log,
		db:           database,
		metrics:      m,
		auth:         authSvc,
		files:        filesSvc,
		shares:       sharesSvc,
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
//	requestID -> recovery -> observability -> securityHeaders -> bodyLimit -> mux
//
// recovery sits outside observability so a panic still produces a metric and a
// log line for the request that caused it; it sits inside requestID so the
// panic log carries the correlation ID.
//
// securityHeaders sits inside observability so the headers are on the response
// recorder the metrics read, and outside the mux so a 404 carries them too — an
// unmatched path is exactly where a reflected-content mistake would land.
// bodyLimit is innermost, closest to the handler that reads the body.
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
	// Wrapped once per route at registration, not once per request: the previous
	// form rebuilt the middleware chain inside the handler body, so every
	// rate-limited request allocated a closure and an http.HandlerFunc before
	// doing any work — on the login path, which is the one under attack when it
	// matters.
	rl := func(h http.HandlerFunc) http.HandlerFunc {
		return s.withRateLimit(h).ServeHTTP
	}

	mux.HandleFunc("GET /api/v1/auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /api/v1/auth/register/begin", rl(s.handleRegisterBegin))
	mux.HandleFunc("POST /api/v1/auth/register/finish", rl(s.handleRegisterFinish))
	mux.HandleFunc("POST /api/v1/auth/login/begin", rl(s.handleLoginBegin))
	mux.HandleFunc("POST /api/v1/auth/login/finish", rl(s.handleLoginFinish))
	mux.HandleFunc("POST /api/v1/auth/recovery/redeem", rl(s.handleRecoveryRedeem))
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)

	// OIDC single sign-on: an additional login path, present only when configured.
	// Unauthenticated (they ARE the login) and rate limited like the rest of auth.
	mux.HandleFunc("GET /api/v1/auth/oidc/login", rl(s.handleOIDCLogin))
	mux.HandleFunc("GET /api/v1/auth/oidc/callback", rl(s.handleOIDCCallback))

	// The sync client's way in: an app password (Basic auth) exchanged for a
	// device bearer token. Rate limited — it verifies a secret, the one online
	// guessing surface a headless client opens.
	mux.HandleFunc("POST /api/v1/auth/token", rl(s.handleCreateDeviceToken))

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

	// Version history: list, restore a past version as a new head, and fetch one
	// past version's bytes without restoring it first.
	mux.HandleFunc("GET /api/v1/nodes/{id}/versions", s.requireAuth(s.handleListVersions))
	mux.HandleFunc("POST /api/v1/nodes/{id}/versions/{versionId}/restore", s.requireAuth(s.handleRestoreVersion))
	mux.HandleFunc("GET /api/v1/nodes/{id}/versions/{versionId}/content", s.requireAuth(s.handleDownloadVersion))
	mux.HandleFunc("HEAD /api/v1/nodes/{id}/versions/{versionId}/content", s.requireAuth(s.handleDownloadVersion))

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
	mux.HandleFunc("GET /api/v1/search", s.requireAuth(s.handleSearch))

	// --- tags ----------------------------------------------------------------
	// Auto tags are applied by the worker; users curate on top. A node's tags
	// also ride along in its single-node GET.
	mux.HandleFunc("GET /api/v1/nodes/{id}/tags", s.requireAuth(s.handleListNodeTags))
	mux.HandleFunc("POST /api/v1/nodes/{id}/tags", s.requireAuth(s.handleAddNodeTag))
	mux.HandleFunc("DELETE /api/v1/nodes/{id}/tags/{tag}", s.requireAuth(s.handleRemoveNodeTag))
	mux.HandleFunc("GET /api/v1/tags", s.requireAuth(s.handleListTags))
	mux.HandleFunc("GET /api/v1/tags/{tag}", s.requireAuth(s.handleTagNodes))

	// --- sync: the change journal cursor -------------------------------------
	mux.HandleFunc("GET /api/v1/changes", s.requireAuth(s.handleChanges))

	// --- sync: the delta protocol --------------------------------------------
	// Block-level transfer: fetch a file's manifest, negotiate which chunks are
	// missing, move only those, and commit a manifest into a file.
	mux.HandleFunc("GET /api/v1/nodes/{id}/manifest", s.requireAuth(s.handleFileManifest))
	mux.HandleFunc("POST /api/v1/chunks/have", s.requireAuth(s.handleHaveChunks))
	mux.HandleFunc("GET /api/v1/chunks/{hash}", s.requireAuth(s.handleGetChunk))
	mux.HandleFunc("PUT /api/v1/chunks/{hash}", s.requireAuth(s.handlePutChunk))
	mux.HandleFunc("POST /api/v1/manifests", s.requireAuth(s.handleCommitManifest))

	// --- shares: management (authenticated) ----------------------------------
	mux.HandleFunc("POST /api/v1/nodes/{id}/shares", s.requireAuth(s.handleCreateShare))
	mux.HandleFunc("GET /api/v1/shares", s.requireAuth(s.handleListShares))
	mux.HandleFunc("DELETE /api/v1/shares/{id}", s.requireAuth(s.handleRevokeShare))

	// --- shares: public plane (UNAUTHENTICATED, rate limited) ----------------
	// The only routes reachable without a session. Rate limited because they are
	// internet-facing and password unlock is a guessing target; each is its own
	// surface, and Caddy's share site block exposes ONLY these under /api.
	mux.HandleFunc("GET /api/v1/s/{token}", rl(s.handleShareView))
	mux.HandleFunc("POST /api/v1/s/{token}/unlock", rl(s.handleShareUnlock))
	mux.HandleFunc("GET /api/v1/s/{token}/content", rl(s.handleShareContent))
	mux.HandleFunc("HEAD /api/v1/s/{token}/content", rl(s.handleShareContent))

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

	return withRequestID(s.withRecovery(s.withObservability(withSecurityHeaders(withBodyLimit(mux)))))
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
	// No "phase" field. It was hardcoded to "1" and stayed there through four
	// phases, so it reported the wrong thing for longer than it reported the
	// right one. Version and commit already identify a build precisely; a
	// hand-maintained label that duplicates them can only drift.
	writeJSON(w, r, http.StatusOK, map[string]any{
		"service": "private-cloud-api",
		"version": s.version,
		"commit":  s.commit,
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
