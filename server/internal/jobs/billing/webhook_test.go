package billing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The webhook is the one part of this package that talks to something outside
// the deployment, so it is the part whose failure modes are worth pinning: a
// signature a receiver can actually verify, a retry that gives a restarting
// receiver time to come back, a refusal that stops instead of hammering, and —
// the default — complete silence when nothing is configured.

func testWebhook(t *testing.T, url string) *Webhook {
	t.Helper()
	w := NewWebhook(Config{URL: url, Secret: "a-secret-of-adequate-length", Attempts: 3}, nil)
	if w == nil {
		t.Fatal("NewWebhook returned nil for a configured endpoint")
	}
	// Backoff collapsed for the test. The production schedule is seconds, and a
	// test that waits it out is a test somebody eventually deletes.
	w.backoff = func(int) time.Duration { return time.Millisecond }
	return w
}

func TestWebhookIsDisabledWithNoEndpoint(t *testing.T) {
	// The default deployment. A nil *Webhook has to be a valid, silent one, or
	// every call site grows a guard and one of them forgets.
	var none *Webhook
	if none.Enabled() {
		t.Error("a nil webhook reports itself enabled")
	}
	if err := none.Deliver(context.Background(), EventPlanChanged, nil); err != nil {
		t.Errorf("delivering on a nil webhook returned %v, want a silent nil", err)
	}
	none.Notify(context.Background(), EventPlanChanged, nil)
	none.Wait()

	if NewWebhook(Config{}, nil) != nil {
		t.Error("NewWebhook built a sender with no URL; off must be off")
	}
}

// The signature is what makes a delivery attributable. It is checked here the
// way a receiver would check it — through the exported Verify — so the
// documented verification is executable rather than prose.
func TestDeliverySignsTheBodyWithItsTimestamp(t *testing.T) {
	type capture struct {
		body      []byte
		timestamp string
		signature string
		event     string
		delivery  string
	}
	got := make(chan capture, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- capture{
			body:      body,
			timestamp: r.Header.Get(TimestampHeader),
			signature: r.Header.Get(SignatureHeader),
			event:     r.Header.Get(EventHeader),
			delivery:  r.Header.Get(DeliveryHeader),
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	hook := testWebhook(t, srv.URL)
	if err := hook.Deliver(context.Background(), EventPlanChanged, map[string]any{"user_id": "u1"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	c := <-got
	if c.event != EventPlanChanged {
		t.Errorf("%s = %q, want %q", EventHeader, c.event, EventPlanChanged)
	}
	if c.delivery == "" {
		t.Errorf("%s is empty; a receiver cannot deduplicate a retry without it", DeliveryHeader)
	}

	sig := strings.TrimPrefix(c.signature, "v1=")
	if sig == c.signature {
		t.Errorf("signature %q is not version-prefixed; a future v2 would be indistinguishable", c.signature)
	}
	if !Verify([]byte("a-secret-of-adequate-length"), c.timestamp, c.body, sig) {
		t.Error("the signature does not verify against the body and timestamp that were sent")
	}
	// The timestamp is inside the signed material, which is what makes a stale
	// delivery unforgeable: moving it forward must invalidate the MAC.
	if Verify([]byte("a-secret-of-adequate-length"), "1", c.body, sig) {
		t.Error("the signature verifies under a different timestamp — a captured delivery could be replayed forever")
	}
	if Verify([]byte("a-different-secret-entirely"), c.timestamp, c.body, sig) {
		t.Error("the signature verifies under the wrong secret")
	}

	var payload map[string]any
	if err := json.Unmarshal(c.body, &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if payload["event"] != EventPlanChanged {
		t.Errorf("body event = %v", payload["event"])
	}
	if _, ok := payload["data"].(map[string]any); !ok {
		t.Error("the payload carries no data object")
	}
}

// A receiver restarting answers 503 for a few seconds. That is exactly what the
// retries are for, and a hook that gave up on the first one would drop events
// during every deploy of the thing receiving them.
func TestDeliveryRetriesATemporaryFailureAndSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := testWebhook(t, srv.URL).Deliver(context.Background(), EventPeriodClosed, nil); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("made %d attempt(s), want 3 — two refusals then the success", n)
	}
}

// The other half of the retry decision, and the one that matters more: a 4xx is
// the receiver saying the request is wrong. Repeating it unchanged cannot make
// it right, and a best-effort hook that hammers an endpoint which has already
// refused is an outage this server caused.
func TestDeliveryDoesNotRetryAPermanentRefusal(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	err := testWebhook(t, srv.URL).Deliver(context.Background(), EventQuotaExceeded, nil)
	if err == nil {
		t.Fatal("a 400 was reported as a successful delivery")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d attempt(s) against a 400, want exactly 1", n)
	}
}

// 429 is the receiver saying "not now", which is the one thing a retry answers —
// so it must be treated as temporary even though it is a 4xx.
func TestDeliveryRetriesRateLimiting(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := testWebhook(t, srv.URL).Deliver(context.Background(), EventPlanChanged, nil); err != nil {
		t.Fatalf("Deliver after a 429: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("made %d attempt(s), want 2", n)
	}
}

// When every attempt fails the event is dropped and reported, never retried
// forever. The caller is expected to log it: an undelivered event is a gap in
// somebody else's record, and blocking on it would break a feature here.
func TestDeliveryGivesUpAfterItsBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := testWebhook(t, srv.URL).Deliver(context.Background(), EventPeriodClosed, nil); err == nil {
		t.Fatal("three failed attempts were reported as a delivery")
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("made %d attempt(s), want the configured 3", n)
	}
}

// Notify detaches. The caller's context being cancelled the instant afterwards —
// an admin closing the tab — must not cancel the delivery, exactly as it must
// not cancel the audit-log write.
func TestNotifyOutlivesTheCallersContext(t *testing.T) {
	delivered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hook := testWebhook(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	hook.Notify(ctx, EventPlanChanged, map[string]any{"user_id": "u1"})
	cancel()

	hook.Wait()
	select {
	case <-delivered:
	default:
		t.Error("the delivery was cancelled with the caller's context")
	}
}
