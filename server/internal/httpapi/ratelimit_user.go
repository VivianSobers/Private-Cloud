package httpapi

import "net/http"

// Per-user rate limiting.
//
// This is the item deferred since Phase 4 slice 5, and the premise that made
// deferring it right is gone. It was correct at one trusted user: a per-IP
// limiter on the auth paths stopped online guessing, `OwnerQueueCap` stopped a
// job flood, and there was nobody else to protect. Phase 7 made a second account
// real, and Phase 8 made a single authenticated request able to spend GPU time
// on a shared sidecar against content the caller may not own. From there, "one
// authenticated user cannot be limited" means one account can degrade the
// service for every other account without doing anything a handler would refuse.
//
// The limiter is keyed by user id, not by address: at this point in the request
// there IS an authenticated identity, so one heavy caller can be slowed without
// touching anybody else — the property the auth limiter cannot have, since its
// whole job is guarding the endpoints that establish who you are. It is applied
// in requireAuth, which is the one place every authenticated route passes
// through, so a route added tomorrow is limited without anyone remembering to
// wrap it.
//
// Still in-process, and still correct for a single node for the reason recorded
// on rateLimiter: if the API is ever replicated this has to move to shared
// state, and until then an external store would buy a property that does not
// exist here.

// routeCost is how much of a shared resource one request to a route can spend.
type routeCost int

const (
	// costNormal is metadata work: a query or two against the caller's own rows.
	// Bounded generously — the ceiling exists to stop a scripted flood, not to
	// pace a person or a busy client.
	costNormal routeCost = iota

	// costHeavy spends something the caller does not own: an RPC to the
	// inference sidecar, which is shared, often GPU-bound, and has no
	// concurrency limit of its own. A loop here is felt on another machine and
	// by every other account.
	costHeavy

	// costStream is the bulk data plane — bytes in and out. Not counted, because
	// counting requests is the wrong meter for it: a large sync legitimately
	// issues one request per chunk, and the things that actually bound this plane
	// are quota, disk and the link itself. Rate-limiting it by request count
	// would throttle a first sync while doing nothing about the bytes.
	costStream
)

// routeCosts names every route that is NOT costNormal.
//
// Default-normal is deliberate: an unclassified route is limited rather than
// exempt, so forgetting to classify one can only ever be too strict, never a
// hole. Getting it wrong shows up as a client that is being slowed and says so,
// which is a failure that reports itself.
//
// Keys are the mux patterns exactly as registered — the same strings Routes()
// returns — and a test fails on an entry that names no registered route, so this
// table cannot quietly rot the way a hand-kept copy of a routing table does.
// These reach a sidecar: a shared, often GPU-bound service with no concurrency
// limit of its own, so a loop here is felt on another machine.
var heavyRoutes = []string{
	"POST /api/v1/chat",
	"GET /api/v1/nodes/{id}/similar",
	"POST /api/v1/admin/fsck",
}

// These move bytes. The last one is the change journal, which is not bulk but
// belongs here for a different reason: it is how a client finds out anything
// changed at all, and a client rate limited off its own change feed cannot even
// discover that it is behind.
var streamRoutes = []string{
	"GET /api/v1/nodes/{id}/content",
	"HEAD /api/v1/nodes/{id}/content",
	"GET /api/v1/nodes/{id}/versions/{versionId}/content",
	"HEAD /api/v1/nodes/{id}/versions/{versionId}/content",
	"POST /api/v1/upload",
	"OPTIONS /api/v1/uploads",
	"POST /api/v1/uploads",
	"HEAD /api/v1/uploads/{id}",
	"PATCH /api/v1/uploads/{id}",
	"DELETE /api/v1/uploads/{id}",
	"GET /api/v1/nodes/{id}/manifest",
	"POST /api/v1/chunks/have",
	"GET /api/v1/chunks/{hash}",
	"PUT /api/v1/chunks/{hash}",
	"POST /api/v1/manifests",
	"GET /api/v1/changes",
}

var routeCosts = buildRouteCosts()

func buildRouteCosts() map[string]routeCost {
	costs := make(map[string]routeCost, len(heavyRoutes)+len(streamRoutes))
	for _, pattern := range heavyRoutes {
		costs[pattern] = costHeavy
	}
	for _, pattern := range streamRoutes {
		costs[pattern] = costStream
	}
	return costs
}

// costOf classifies the route a request matched.
//
// r.Pattern is set by the mux, so this is the registered pattern rather than the
// raw path — a filename with a slash in it cannot masquerade as another route.
// An empty pattern (a handler invoked directly, as some unit tests do) falls
// back to costNormal, which is the limited case.
func costOf(r *http.Request) routeCost {
	return routeCosts[r.Pattern]
}

// userLimited applies the per-user budget for the route's cost class. It reports
// false when the request has been refused and a response already written.
func (s *Server) userAllowed(w http.ResponseWriter, r *http.Request, userID string) bool {
	var (
		limiter *rateLimiter
		retry   string
		message string
	)

	switch costOf(r) {
	case costStream:
		return true
	case costHeavy:
		limiter, retry = s.heavyLimiter, "10"
		message = "too many requests to the intelligence endpoints; they run on a shared model service, so slow down and try again shortly"
	default:
		limiter, retry = s.userLimiter, "5"
		message = "too many requests; slow down and try again shortly"
	}

	if limiter == nil || limiter.allow("user:"+userID) {
		return true
	}

	w.Header().Set("Retry-After", retry)
	writeError(w, r, http.StatusTooManyRequests, "rate_limited", message)
	return false
}
