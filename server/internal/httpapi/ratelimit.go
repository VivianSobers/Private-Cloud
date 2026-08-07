package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
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

	// stop ends the reaper goroutine. Process-lifetime limiters never close it;
	// tests do, so a package that constructs a limiter per case does not leak a
	// goroutine and a ticker for each one.
	stop chan struct{}
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
		stop:     make(chan struct{}),
	}
	go rl.reap()
	return rl
}

// close ends the reaper. Safe to call once; the server's own limiters live for
// the life of the process and never do.
func (rl *rateLimiter) close() { close(rl.stop) }

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
// A ticker that is stopped rather than time.Tick, which allocates a ticker that
// can never be collected — fine for one process-lifetime limiter, a leak per
// instance for anything that constructs them repeatedly.
func (rl *rateLimiter) reap() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-rl.stop:
			return
		case <-ticker.C:
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

// clientIP extracts the address to rate limit on.
//
// X-Forwarded-For is honoured ONLY when the peer is a configured trusted proxy,
// and never otherwise. Both halves matter:
//
//   - Trusting it unconditionally lets anyone reset their own limit by forging a
//     new client address on each request, which is worse than no limiter.
//   - Distrusting it unconditionally — the previous behaviour — means every
//     request arrives from Caddy, so ALL traffic shares one bucket. One client
//     burning 20 requests a minute then locks every other user out of login,
//     recovery, device-token exchange and every public share unlock at once.
//     That was a fair trade for a single-user tailnet and stopped being one when
//     Phase 4 added SSO and multiple accounts.
//
// The rightmost entry is taken, not the leftmost: everything to the left is
// client-supplied and forgeable, while the last entry is the address the trusted
// proxy itself observed.
func clientIP(r *http.Request) string {
	return clientIPFrom(r, defaultTrustedProxies)
}

func clientIPFrom(r *http.Request, trusted []netip.Prefix) string {
	peer := peerHost(r)
	if len(trusted) == 0 || !isTrusted(peer, trusted) {
		return peer
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		addr, err := netip.ParseAddr(candidate)
		if err != nil {
			continue
		}
		// Walk left past any further trusted hops, so a chain of proxies still
		// resolves to the first address none of them vouched for.
		if isTrusted(addr.String(), trusted) {
			continue
		}
		return addr.String()
	}
	return peer
}

func peerHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isTrusted(host string, trusted []netip.Prefix) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// defaultTrustedProxies is the set of peers whose X-Forwarded-For is believed.
//
// Loopback and the RFC 1918 / ULA ranges: in every supported deployment the only
// thing that can reach the API is Caddy, on the same host or the same private
// Docker network. Nothing routable from outside is in here, so a request that
// genuinely arrives from the internet is limited on its own address and can
// never forge another.
var defaultTrustedProxies = mustPrefixes(
	"127.0.0.0/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"fc00::/7",
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			panic("ratelimit: bad built-in CIDR " + c + ": " + err.Error())
		}
		out = append(out, p)
	}
	return out
}
