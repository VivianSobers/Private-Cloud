package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a token-bucket limiter keyed by client IP.
//
// In-process and non-distributed, which is correct for a single-node
// deployment: an external store would add a dependency to the auth path in
// exchange for a property (shared state across replicas) that does not exist
// here. If the API ever runs replicated, this must move.
//
// It exists specifically to blunt online guessing of recovery codes. Passkeys
// are not guessable, but a 100-bit recovery code still deserves not to be
// attackable at wire speed.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	rate     float64 // tokens per second
	capacity float64 // burst
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute, burst float64) *rateLimiter {
	rl := &rateLimiter{
		buckets:  make(map[string]*bucket),
		rate:     perMinute / 60,
		capacity: burst,
	}
	go rl.reap()
	return rl
}

// allow consumes a token, reporting whether the request may proceed.
func (rl *rateLimiter) allow(key string) bool {
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		rl.buckets[key] = &bucket{tokens: rl.capacity - 1, last: now}
		return true
	}

	// Refill for elapsed time, capped at capacity.
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if b.tokens > rl.capacity {
		b.tokens = rl.capacity
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// reap drops idle buckets so the map cannot grow without bound — otherwise a
// scanner cycling source addresses would be a slow memory leak.
func (rl *rateLimiter) reap() {
	for range time.Tick(5 * time.Minute) {
		cutoff := time.Now().Add(-15 * time.Minute)
		rl.mu.Lock()
		for k, b := range rl.buckets {
			if b.last.Before(cutoff) {
				delete(rl.buckets, k)
			}
		}
		rl.mu.Unlock()
	}
}

// withRateLimit guards the auth endpoints.
func (s *Server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authLimiter.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, r, http.StatusTooManyRequests, "rate_limited",
				"too many authentication attempts; wait a minute and try again")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// searchLimited guards the search endpoint.
//
// Search is the one authenticated route whose cost is not paid in the caller's
// own data. A semantic query is an RPC to the inference sidecar — a shared,
// serialised, often GPU-bound service with no auth and no concurrency limit of
// its own — so a single session in a loop can saturate it for every other user,
// and on the split-tier deployment can saturate a whole other machine.
//
// Keyed per user rather than per IP, unlike the auth limiter: here we HAVE an
// authenticated identity, so one heavy user can be slowed without touching
// anyone else. The auth limiter cannot do that, because its whole purpose is to
// guard the endpoints that establish who you are.
//
// The budget is deliberately loose. The web client debounces at 200ms, so a fast
// typist generates a few queries a second in bursts; 120/min with a burst of 30
// is invisible to that and still bounds a scripted loop.
func (s *Server) searchLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := clientIP(r)
		if u := CurrentUser(r.Context()); u != nil {
			key = "user:" + u.ID.String()
		}
		if !s.searchLimiter.allow(key) {
			w.Header().Set("Retry-After", "10")
			writeError(w, r, http.StatusTooManyRequests, "rate_limited",
				"too many searches; slow down and try again shortly")
			return
		}
		next(w, r)
	}
}

// clientIP extracts the peer address.
//
// It deliberately does NOT trust X-Forwarded-For. Caddy is the only ingress and
// it terminates every connection, so the peer address is always Caddy — and an
// attacker-supplied XFF header would otherwise let anyone reset their own rate
// limit by forging a new client IP each request. The cost is that all traffic
// shares one bucket; on a single-user tailnet that is an acceptable trade, and
// it is the safe direction to be wrong in.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
