package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/config"
)

// fakeServer serves just enough of the API for the preflight: an unauthenticated
// status endpoint, the token exchange (gated on a password), and the tree root.
func fakeServer(t *testing.T, goodPassword string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/status", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		_, pass, _ := r.BasicAuth()
		if pass != goodPassword {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"bad credential"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":"tok","expires_at":"2999-01-01T00:00:00Z"}`))
	})
	mux.HandleFunc("/api/v1/nodes/root", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"node":{"id":"root","kind":"folder","name":"","path":"/"}}`))
	})
	mux.HandleFunc("/api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.0.0","commit":"abc123"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func cfgFor(t *testing.T, serverURL, password string) *config.Config {
	root := t.TempDir()
	return &config.Config{
		ServerURL:   serverURL,
		Username:    "vivian",
		AppPassword: password,
		Root:        root,
		StateDB:     filepath.Join(root, ".pcsync", "state.db"),
	}
}

func statusOf(r Result, name string) Status {
	for _, c := range r.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	return Fail
}

// A healthy setup passes every check.
func TestAllHealthy(t *testing.T) {
	ts := fakeServer(t, "correct-password")
	// A client version equal to the server's makes the version check a clean pass.
	res := Run(context.Background(), cfgFor(t, ts.URL, "correct-password"), "pcsync-test", "1.0.0")
	if !res.OK() {
		t.Fatalf("expected all checks to pass, got %+v", res.Checks)
	}
	for _, name := range []string{"configuration", "state database", "server reachable", "sign in", "client version"} {
		if statusOf(res, name) != Pass {
			t.Errorf("%q did not pass", name)
		}
	}
}

// A wrong app password reaches the server but fails sign-in specifically — the
// diagnostic that points at the credential, not the network.
func TestBadCredential(t *testing.T) {
	ts := fakeServer(t, "correct-password")
	res := Run(context.Background(), cfgFor(t, ts.URL, "WRONG"), "pcsync-test", "1.0.0")
	if res.OK() {
		t.Fatal("expected the preflight to fail")
	}
	if statusOf(res, "server reachable") != Pass {
		t.Error("server should still be reachable with a bad credential")
	}
	if statusOf(res, "sign in") != Fail {
		t.Error("sign in should fail with a bad credential")
	}
}

// An unreachable server fails reachability (and, consequently, sign-in), without
// blaming the credential.
func TestUnreachable(t *testing.T) {
	// Port 1 is reserved and never listening, so the dial fails fast.
	res := Run(context.Background(), cfgFor(t, "http://127.0.0.1:1", "x"), "pcsync-test", "1.0.0")
	if statusOf(res, "server reachable") != Fail {
		t.Error("expected reachability to fail")
	}
	if statusOf(res, "state database") != Pass {
		t.Error("the local state DB check should still pass — it is not network-bound")
	}
}

// A client behind the server warns — invites an update — but never fails the
// preflight, because a version skew does not stop a sync.
func TestVersionSkew(t *testing.T) {
	ts := fakeServer(t, "correct-password") // server reports 1.0.0
	res := Run(context.Background(), cfgFor(t, ts.URL, "correct-password"), "pcsync-test", "0.9.0")
	if statusOf(res, "client version") != Warn {
		t.Errorf("expected a version-skew warning, got %v", statusOf(res, "client version"))
	}
	if !res.OK() {
		t.Error("a version skew must not fail the preflight")
	}
}

// A dev build cannot be compared to a release tag, so the check passes softly
// rather than warning about a difference that means nothing.
func TestVersionDevBuildSkipped(t *testing.T) {
	ts := fakeServer(t, "correct-password")
	res := Run(context.Background(), cfgFor(t, ts.URL, "correct-password"), "pcsync-test", "dev")
	if statusOf(res, "client version") != Pass {
		t.Errorf("a dev build should pass the version check, got %v", statusOf(res, "client version"))
	}
}
