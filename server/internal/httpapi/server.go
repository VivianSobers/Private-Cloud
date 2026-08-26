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
	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs/billing"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/metrics"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/push"
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

	// generator is set when a generation sidecar is configured; nil otherwise, in
	// which case /chat still answers with retrieved passages and no written
	// answer. Retrieval is the trustworthy half of RAG and useful on its own, so
	// the absence of an optional generator must not take away a working feature.
	generator embed.Generator

	// imageEmbedder is set when an image-embedding sidecar is configured; nil
	// otherwise, in which case /similar ranks in the document space exactly as it
	// did before the image space existed. Held for its MODEL and width only —
	// ranking reads vectors the worker already stored, so no request path calls
	// this and it has no timeout anyone waits on.
	imageEmbedder embed.ImageEmbedder

	// oidc is set when a single sign-on provider is configured; nil otherwise, in
	// which case the OIDC endpoints report the feature disabled and the passkey
	// paths are entirely unaffected.
	oidc *auth.OIDCProvider

	// oidcFlowKey authenticates the short-lived login-flow cookie, so the state
	// the callback compares against cannot be planted by whoever can set a cookie
	// on the victim's browser. Minted per process rather than persisted: the
	// cookie lives ten minutes and exists only between the redirect out and the
	// redirect back, so the worst a restart costs is one login retry — cheaper
	// than another secret to manage and rotate.
	oidcFlowKey []byte

	// Guards the auth endpoints against online guessing. 20/min with a burst
	// of 10 is invisible to a human signing in and useless to a script.
	authLimiter *rateLimiter

	// Guards search, which is the one authenticated route whose cost lands on a
	// shared resource — the inference sidecar — rather than on the caller's own
	// data. Keyed per user, and loose enough that the debounced search box in the
	// web client never meets it.
	searchLimiter *rateLimiter

	// The general per-user budget on the authenticated API, applied in
	// requireAuth. Generous: it bounds a scripted flood without ever being felt
	// by a person or by a busy sync client.
	userLimiter *rateLimiter

	// The tight per-user budget on the routes that spend GPU time on a shared
	// sidecar. See ratelimit_user.go.
	heavyLimiter *rateLimiter

	// Web Push. nil when no VAPID key is configured, which is a supported state:
	// GET /push/key then 404s and a client keeps polling GET /changes.
	push     *push.Sender
	notifier *push.Notifier

	// billingHook delivers plan-change events to whatever an operator has
	// pointed it at. nil when unconfigured, which is the default and a fully
	// supported state — a nil *billing.Webhook is a valid disabled one, so no
	// call site has to guard and the disabled path cannot be the one somebody
	// forgets to test.
	billingHook *billing.Webhook

	// blobs is the tiered store, when this deployment has a cold tier. Held only
	// so the storage report can say whether one exists and how much is in it —
	// every actual read goes through files.Service, which has the same store.
	// Nil is the ordinary case and reads as "no cold tier".
	blobs *blob.TieredStore
}

// SetColdTier tells the storage report that a cold tier exists.
//
// Separate from the blob store the file service was built with, because this is
// the only thing in the HTTP layer that needs to KNOW about tiering rather than
// simply read through it. Left unset, /admin/storage reports the tier as
// disabled — which is the honest answer for every deployment that has none.
func (s *Server) SetColdTier(t *blob.TieredStore) { s.blobs = t }

// SetPush enables Web Push delivery. Left unset when no VAPID key is configured.
//
// Both halves land together on purpose. Publishing a key without a sender would
// let a browser subscribe to something that never delivers, and a sender with no
// published key can have no subscriptions to deliver to; either alone is a
// feature that looks present and is not.
func (s *Server) SetPush(sender *push.Sender) {
	s.push = sender
	s.notifier = push.NewNotifier(sender, pushTargets{store: s.auth.Store()}, s.files.Store(), s.log)
}

// SetEmbedder enables semantic search by wiring the query embedder. Left unset
// when no inference sidecar is configured.
func (s *Server) SetEmbedder(e embed.Embedder) { s.embedder = e }

// SetGenerator enables written answers on /chat. Left unset when no generation
// sidecar is configured, in which case the endpoint returns retrieval only.
func (s *Server) SetGenerator(g embed.Generator) { s.generator = g }

// SetImageEmbedder enables image-space similarity by naming the space photos are
// indexed in. Left unset when no image sidecar is configured, in which case
// /similar keeps ranking in the document space and a photograph with no text
// still answers 404 not_indexed — the behaviour that predates the space.
func (s *Server) SetImageEmbedder(e embed.ImageEmbedder) { s.imageEmbedder = e }

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
		// 120/min, burst 30: the web client debounces at 200ms, so even a fast
		// typist stays well inside this, while a scripted loop against the
		// sidecar is bounded.
		searchLimiter: newRateLimiter(120, 30),
		// 1200/min, burst 600. A person browsing generates a handful of requests
		// a second at most and a sync client's metadata traffic is a poll every
		// few seconds, so this is invisible to both; a script hammering an
		// endpoint is held to 20/s, which is the point.
		userLimiter: newRateLimiter(1200, 600),
		// 30/min, burst 15 for the sidecar-spending routes. A person asking
		// questions of their library asks a few a minute; this bounds a loop that
		// would otherwise occupy a GPU box on somebody else's behalf.
		heavyLimiter: newRateLimiter(30, 15),
		oidcFlowKey:  newFlowKey(),
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
	s.registerRoutes(mux)

	return withRequestID(s.withRecovery(s.withObservability(withSecurityHeaders(withBodyLimit(mux)))))
}

// router is the part of *http.ServeMux the route table uses.
//
// It exists so the same registration code can be replayed into something that
// only records patterns instead of into a live mux — see Routes. One route
// table with two consumers, rather than a second hand-maintained copy that
// would be wrong the first time somebody added a route and forgot it.
type router interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Routes returns every pattern this server registers, in registration order.
//
// Derived by replaying the real route table, never by copying it, which is what
// makes it usable as the source for the generated OpenAPI document: the spec
// cannot claim an endpoint the mux does not serve, or miss one it does.
func (s *Server) Routes() []string {
	var rec patternRecorder
	s.registerRoutes(&rec)
	return rec.patterns
}

type patternRecorder struct{ patterns []string }

func (p *patternRecorder) Handle(pattern string, _ http.Handler) {
	p.patterns = append(p.patterns, pattern)
}

func (p *patternRecorder) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	p.patterns = append(p.patterns, pattern)
}

// registerRoutes declares the entire HTTP surface. Everything the server serves
// is registered here and nowhere else.
func (s *Server) registerRoutes(mux router) {
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
	// Search is rate limited even though it is authenticated. Every other
	// authenticated route costs a query against the caller's own data; a semantic
	// query costs an RPC to the inference sidecar, which is a shared, serialised,
	// possibly GPU-bound resource that one session can otherwise saturate for
	// everyone. The limiter is generous enough that a person typing into the
	// debounced search box never meets it.
	// requireAuth is OUTSIDE the limiter deliberately: the limiter keys on the
	// authenticated user, so authentication has to have happened first.
	mux.HandleFunc("GET /api/v1/search", s.requireAuth(s.searchLimited(s.handleSearch)))

	// --- tags ----------------------------------------------------------------
	// Auto tags are applied by the worker; users curate on top. A node's tags
	// also ride along in its single-node GET.
	mux.HandleFunc("GET /api/v1/nodes/{id}/tags", s.requireAuth(s.handleListNodeTags))
	mux.HandleFunc("POST /api/v1/nodes/{id}/tags", s.requireAuth(s.handleAddNodeTag))
	mux.HandleFunc("DELETE /api/v1/nodes/{id}/tags/{tag}", s.requireAuth(s.handleRemoveNodeTag))
	mux.HandleFunc("GET /api/v1/tags", s.requireAuth(s.handleListTags))
	mux.HandleFunc("GET /api/v1/tags/{tag}", s.requireAuth(s.handleTagNodes))

	// --- intelligence (Phase 8) ----------------------------------------------
	// Similarity rides the embedding space Phase 4 already built, so it needs no
	// new job kind and degrades exactly as /search?semantic=true does.
	mux.HandleFunc("GET /api/v1/nodes/{id}/similar", s.requireAuth(s.searchLimited(s.handleSimilar)))

	// Chat is rate limited alongside search for the same reason and more so: it
	// costs an embedding RPC AND a generation pass, the most expensive thing one
	// authenticated user can ask this deployment to do.
	mux.HandleFunc("POST /api/v1/chat", s.requireAuth(s.searchLimited(s.handleChat)))

	// Feedback on what the machine produced. NOT rate limited alongside search:
	// it costs one small write against the caller's own rows and no sidecar at
	// all, and slowing down the one route whose entire purpose is to hear that
	// something is wrong would be the wrong thing to ration. It is also the only
	// Phase 8 route that works with every sidecar switched off, which is why it
	// carries no availability check.
	mux.HandleFunc("POST /api/v1/feedback", s.requireAuth(s.handleSubmitFeedback))
	mux.HandleFunc("GET /api/v1/feedback", s.requireAuth(s.handleListFeedback))

	// People. A cluster is unnamed until somebody names it; merge and reassign
	// exist because clustering is going to be wrong, and a faces feature with no
	// correction path is one people stop trusting after the first mistake.
	mux.HandleFunc("GET /api/v1/people", s.requireAuth(s.handleListPeople))
	mux.HandleFunc("GET /api/v1/people/{id}", s.requireAuth(s.handleGetPerson))
	mux.HandleFunc("PATCH /api/v1/people/{id}", s.requireAuth(s.handlePatchPerson))
	mux.HandleFunc("POST /api/v1/people/{id}/merge", s.requireAuth(s.handleMergePeople))
	mux.HandleFunc("DELETE /api/v1/people/{id}", s.requireAuth(s.handleForgetPerson))
	mux.HandleFunc("GET /api/v1/nodes/{id}/faces", s.requireAuth(s.handleListNodeFaces))
	mux.HandleFunc("POST /api/v1/nodes/{id}/faces/{faceId}/reassign", s.requireAuth(s.handleReassignFace))

	// --- sharing and collaboration (Phase 7) ---------------------------------
	// Shared content stays OUT of the existing endpoints unless a client opts in
	// with ?include_shared=true. Widening the default would silently change what
	// GET /nodes/{id}/children and /search mean for every already-shipped client.
	mux.HandleFunc("GET /api/v1/grants", s.requireAuth(s.handleListGrants))
	mux.HandleFunc("GET /api/v1/nodes/{id}/grants", s.requireAuth(s.handleListNodeGrants))
	mux.HandleFunc("POST /api/v1/nodes/{id}/grants", s.requireAuth(s.handleCreateGrant))
	mux.HandleFunc("PATCH /api/v1/grants/{id}", s.requireAuth(s.handleUpdateGrant))
	mux.HandleFunc("DELETE /api/v1/grants/{id}", s.requireAuth(s.handleDeleteGrant))
	mux.HandleFunc("GET /api/v1/shared", s.requireAuth(s.handleShared))

	// --- devices (Phase 6) ---------------------------------------------------
	// A device is a session of kind 'device'; these routes exist alongside
	// /auth/sessions because a device list wants the CLIENT's identity rather
	// than the session's. Push is a hook — the subscription is stored here and
	// delivered by something else, never by this server.
	mux.HandleFunc("GET /api/v1/devices", s.requireAuth(s.handleListDevices))
	mux.HandleFunc("PATCH /api/v1/devices/{id}", s.requireAuth(s.handlePatchDevice))
	mux.HandleFunc("DELETE /api/v1/devices/{id}", s.requireAuth(s.handleRevokeDevice))
	mux.HandleFunc("GET /api/v1/push/key", s.requireAuth(s.handlePushKey))
	mux.HandleFunc("POST /api/v1/devices/{id}/push", s.requireAuth(s.handleRegisterPush))
	mux.HandleFunc("DELETE /api/v1/devices/{id}/push", s.requireAuth(s.handleUnregisterPush))

	// --- media: timeline and albums (Phase 5) --------------------------------
	// Derived renditions are NOT here: ?variant= is a parameter on the existing
	// content route, so ranges, ETags and the share plane keep working for a
	// thumbnail exactly as they do for an original.
	mux.HandleFunc("GET /api/v1/media/timeline", s.requireAuth(s.handleTimeline))

	mux.HandleFunc("GET /api/v1/albums", s.requireAuth(s.handleListAlbums))
	mux.HandleFunc("POST /api/v1/albums", s.requireAuth(s.handleCreateAlbum))
	mux.HandleFunc("GET /api/v1/albums/{id}", s.requireAuth(s.handleGetAlbum))
	mux.HandleFunc("PATCH /api/v1/albums/{id}", s.requireAuth(s.handlePatchAlbum))
	mux.HandleFunc("DELETE /api/v1/albums/{id}", s.requireAuth(s.handleDeleteAlbum))
	mux.HandleFunc("POST /api/v1/albums/{id}/items", s.requireAuth(s.handleAddAlbumItems))
	// The whole order in one call, so a drag-reorder cannot half-apply.
	mux.HandleFunc("PATCH /api/v1/albums/{id}/items", s.requireAuth(s.handleReorderAlbum))
	mux.HandleFunc("DELETE /api/v1/albums/{id}/items/{nodeId}", s.requireAuth(s.handleRemoveAlbumItem))

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

	// --- admin console (Phase 7) ---------------------------------------------
	// Every route is admin-only server-side. The client gates its nav on
	// is_admin as a convenience; this is the boundary.
	mux.HandleFunc("GET /api/v1/admin/users", s.requireAdmin(s.handleAdminListUsers))
	mux.HandleFunc("POST /api/v1/admin/users", s.requireAdmin(s.handleAdminCreateUser))
	mux.HandleFunc("PATCH /api/v1/admin/users/{id}", s.requireAdmin(s.handleAdminPatchUser))
	// Disables and revokes; never deletes content. See the handler.
	mux.HandleFunc("DELETE /api/v1/admin/users/{id}", s.requireAdmin(s.handleAdminDeleteUser))
	mux.HandleFunc("GET /api/v1/admin/users/{id}/sessions", s.requireAdmin(s.handleAdminUserSessions))
	mux.HandleFunc("DELETE /api/v1/admin/users/{id}/sessions/{sid}", s.requireAdmin(s.handleAdminRevokeUserSession))
	mux.HandleFunc("GET /api/v1/admin/audit", s.requireAdmin(s.handleAdminAudit))

	// --- storage health (Phase 9) --------------------------------------------
	// Reads the same textfile collectors the alerts scrape, so the console and
	// Grafana never disagree about whether the pool is healthy.
	mux.HandleFunc("GET /api/v1/admin/storage", s.requireAdmin(s.handleAdminStorage))

	// --- billing hooks (Phase 9 slice 5) -------------------------------------
	// The integration seam, not a billing system: a plan is a named quota, a
	// metering record makes a closed period answerable, and the outbound webhook
	// is how something else is told. Admin-only, like every other console route,
	// because a plan is what an account is allowed to store and a metering record
	// is how much everybody stored.
	mux.HandleFunc("GET /api/v1/admin/billing/plans", s.requireAdmin(s.handleAdminListPlans))
	mux.HandleFunc("POST /api/v1/admin/billing/plans", s.requireAdmin(s.handleAdminUpsertPlan))
	mux.HandleFunc("PUT /api/v1/admin/billing/accounts/{id}/plan", s.requireAdmin(s.handleAdminSetAccountPlan))
	mux.HandleFunc("GET /api/v1/admin/billing/metering", s.requireAdmin(s.handleAdminMetering))

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
