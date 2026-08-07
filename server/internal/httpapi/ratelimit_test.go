package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// X-Forwarded-For is believed from a trusted peer and ignored from anyone else.
func TestClientIPTrustedProxyOnly(t *testing.T) {
	trusted := mustPrefixes("127.0.0.0/8", "10.0.0.0/8")

	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	cases := []struct {
		name, remote, xff, want string
	}{
		// Caddy on the private network: believe what it observed.
		{"trusted peer", "10.0.0.5:1234", "203.0.113.9", "203.0.113.9"},
		// A forged chain: only the rightmost untrusted hop counts, so the
		// attacker-supplied entries on the left cannot mint a fresh bucket.
		{"forged chain", "10.0.0.5:1234", "1.1.1.1, 2.2.2.2, 203.0.113.9", "203.0.113.9"},
		// Further trusted hops are walked past.
		{"proxy chain", "127.0.0.1:1", "203.0.113.9, 10.0.0.7", "203.0.113.9"},
		// Direct from a routable address: the header is not believed at all.
		{"untrusted peer", "198.51.100.4:5555", "203.0.113.9", "198.51.100.4"},
		// Nothing usable in the header falls back to the peer.
		{"garbage header", "10.0.0.5:1234", "not-an-ip", "10.0.0.5"},
		{"no header", "10.0.0.5:1234", "", "10.0.0.5"},
	}
	for _, c := range cases {
		if got := clientIPFrom(req(c.remote, c.xff), trusted); got != c.want {
			t.Errorf("%s: clientIP = %q, want %q", c.name, got, c.want)
		}
	}
}

// An empty trusted set must never believe the header.
func TestClientIPNoTrustedProxies(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIPFrom(r, nil); got != "10.0.0.5" {
		t.Errorf("clientIP = %q, want the peer address", got)
	}
}

func TestRateLimiterAllowsBurstThenBlocks(t *testing.T) {
	// 60/min = 1/sec, burst 5.
	rl := newRateLimiter(60, 5)

	for i := 0; i < 5; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("request %d within the burst was rejected", i+1)
		}
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("request beyond the burst should have been rejected")
	}
}

func TestRateLimiterIsPerKey(t *testing.T) {
	rl := newRateLimiter(60, 2)

	rl.allow("1.1.1.1")
	rl.allow("1.1.1.1")
	if rl.allow("1.1.1.1") {
		t.Fatal("first key should be exhausted")
	}
	// One client exhausting its bucket must not lock out everyone else.
	if !rl.allow("2.2.2.2") {
		t.Fatal("a different key should have its own bucket")
	}
}

func TestRateLimiterRefills(t *testing.T) {
	// 6000/min = 100/sec, so a 20ms wait restores ~2 tokens.
	rl := newRateLimiter(6000, 2)

	rl.allow("k")
	rl.allow("k")
	if rl.allow("k") {
		t.Fatal("bucket should be empty")
	}

	time.Sleep(30 * time.Millisecond)
	if !rl.allow("k") {
		t.Fatal("bucket should have refilled")
	}
}

func TestRateLimiterDoesNotExceedCapacity(t *testing.T) {
	// After a long idle period the bucket must cap at capacity, not accumulate
	// unbounded credit that would let one client burst enormously.
	rl := newRateLimiter(60000, 3)
	rl.allow("k")

	time.Sleep(20 * time.Millisecond) // would earn ~20 tokens uncapped

	for i := 0; i < 3; i++ {
		if !rl.allow("k") {
			t.Fatalf("token %d should be available up to capacity", i+1)
		}
	}
	if rl.allow("k") {
		t.Fatal("bucket exceeded its capacity after idling")
	}
}

func TestClientIPIgnoresForwardedHeader(t *testing.T) {
	// Trusting X-Forwarded-For would let any client reset its own rate limit by
	// forging a new address on every request.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login/begin", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := clientIP(r); got != "10.0.0.5" {
		t.Errorf("clientIP = %q, want the peer address 10.0.0.5", got)
	}
}

func TestClientIPHandlesMalformedRemoteAddr(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "not-a-host-port"
	if got := clientIP(r); got != "not-a-host-port" {
		t.Errorf("clientIP = %q, want the raw value as a fallback", got)
	}
}

func TestRecoverySessionPathAllowlist(t *testing.T) {
	// A recovery session exists only to enrol a new passkey. If this list ever
	// grows to include file routes, a printed code becomes equivalent to a
	// passkey and the whole point of passkeys is lost.
	allowed := []string{
		"/api/v1/auth/register/begin",
		"/api/v1/auth/register/finish",
		"/api/v1/auth/me",
		"/api/v1/auth/logout",
		"/api/v1/auth/recovery/regenerate",
	}
	for _, p := range allowed {
		if !isRecoveryAllowedPath(p) {
			t.Errorf("path %q should be allowed for recovery sessions", p)
		}
	}

	for _, p := range []string{
		"/api/v1/files",
		"/api/v1/auth/sessions",
		"/api/v1/auth/credentials",
		"/api/v1/version",
		"/",
	} {
		if isRecoveryAllowedPath(p) {
			t.Errorf("path %q must NOT be reachable with a recovery session", p)
		}
	}
}
