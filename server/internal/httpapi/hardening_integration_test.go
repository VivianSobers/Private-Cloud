package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// Every response carries the baseline security headers.
func TestSecurityHeadersPresent(t *testing.T) {
	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, "/healthz", nil, nil)
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
}

// A metadata endpoint refuses an oversized JSON body rather than buffering it —
// the OOM guard on everything that is not a deliberate large-content route.
func TestBodyLimitRejectsHugeJSON(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	huge := strings.Repeat("a", 3<<20) // 3 MiB, over the 2 MiB metadata cap
	body := `{"parent_id":"` + root + `","name":"` + huge + `"}`
	rec := f.do(http.MethodPost, "/api/v1/folders", strings.NewReader(body), f.cookie)
	if rec.Code < 400 {
		t.Errorf("oversized folder-create body accepted: %d", rec.Code)
	}
}
