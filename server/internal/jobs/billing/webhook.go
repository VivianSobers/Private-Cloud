package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The outbound billing webhook — the actual "hook" in "billing hooks", and the
// reason the row can be honest about no payment provider having been chosen.
//
// It is off unless configured, because a self-hosted server with one user has
// nothing to tell anybody and an outbound HTTP call it did not need is a
// capability an attacker would rather it had. When it is on, it is best effort
// and detached from whatever caused it, on exactly the reasoning the audit-log
// write follows: an event that was not delivered is a gap in someone else's
// record, while a plan change refused because a receiver was down is a broken
// feature in this one.

// The events this server emits. Small and closed on purpose: an event set that
// grows to mirror every internal state change becomes a second API nobody
// versioned.
const (
	// EventPlanChanged fires when an account is put on, moved between, or taken
	// off a plan. It carries the quota that was written through, because the
	// quota is the thing that actually changed for the account.
	EventPlanChanged = "plan.changed"
	// EventPeriodClosed fires once, when a billing period ends, carrying the
	// final figure for every account measured in it.
	EventPeriodClosed = "period.closed"
	// EventQuotaExceeded fires the first time a metering sweep sees an account
	// over its limit within a period. See Meter.checkQuotaEvent for why it is
	// observed here rather than hooked onto the 507.
	EventQuotaExceeded = "quota.exceeded"
)

// SignatureHeader carries the HMAC. Named after the scheme rather than the
// product, because a receiver reading `v1=` should be able to tell that a future
// `v2=` is a different construction rather than a different key.
const (
	SignatureHeader = "X-PC-Signature"
	TimestampHeader = "X-PC-Timestamp"
	EventHeader     = "X-PC-Event"
	DeliveryHeader  = "X-PC-Delivery"
)

// Webhook delivers billing events to one configured endpoint.
//
// A nil *Webhook is a valid, disabled webhook: every method tolerates it, so no
// caller has to guard, and the disabled path cannot be the one somebody forgets
// to test.
type Webhook struct {
	url    string
	secret []byte

	client   *http.Client
	attempts int
	backoff  func(attempt int) time.Duration
	log      *slog.Logger
	now      func() time.Time

	// wg tracks detached deliveries so a shutting-down process can wait for them
	// rather than killing a retry mid-flight. Best effort does not mean careless:
	// the one delivery worth waiting a second for is the one already in progress.
	wg sync.WaitGroup
}

// Config is what the environment supplies.
type Config struct {
	URL      string
	Secret   string
	Timeout  time.Duration
	Attempts int
}

// Enabled reports whether an endpoint is configured.
func (c Config) Enabled() bool { return c.URL != "" }

// NewWebhook builds a sender, or returns nil when no endpoint is configured.
func NewWebhook(cfg Config, log *slog.Logger) *Webhook {
	if !cfg.Enabled() {
		return nil
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = 4
	}
	return &Webhook{
		url:      cfg.URL,
		secret:   []byte(cfg.Secret),
		client:   &http.Client{Timeout: cfg.Timeout},
		attempts: cfg.Attempts,
		log:      log,
		now:      time.Now,
		// Doubling from a second: 1s, 2s, 4s — under half a minute for the
		// default four attempts, which is long enough to ride out a receiver
		// restart and short enough that a shutdown does not wait on it.
		backoff: func(attempt int) time.Duration {
			return time.Duration(1<<uint(attempt)) * time.Second
		},
	}
}

// Enabled reports whether this webhook will deliver anything.
func (w *Webhook) Enabled() bool { return w != nil && w.url != "" }

// Notify delivers an event without blocking the caller.
//
// The context is detached with WithoutCancel, exactly as the audit-log write is:
// an admin closing their browser the instant after a successful plan change must
// not cancel the record of it. A timeout is imposed instead, so a detached
// delivery cannot outlive the process's patience.
func (w *Webhook) Notify(ctx context.Context, event string, payload map[string]any) {
	if !w.Enabled() {
		return
	}
	detached := context.WithoutCancel(ctx)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		// Bounded by the total retry budget plus one client timeout, so a
		// pathological receiver cannot hold a goroutine open indefinitely.
		budget := time.Duration(w.attempts) * (w.client.Timeout + 8*time.Second)
		c, cancel := context.WithTimeout(detached, budget)
		defer cancel()
		if err := w.Deliver(c, event, payload); err != nil {
			w.log.Warn("billing webhook not delivered", "event", event, "error", err)
		}
	}()
}

// Wait blocks until every detached delivery has finished. Called on shutdown.
func (w *Webhook) Wait() {
	if w == nil {
		return
	}
	w.wg.Wait()
}

// Deliver sends one event, retrying with backoff, and reports the final outcome.
//
// Exported so the retry behaviour is testable directly rather than through a
// goroutine, and so a future `cloudctl billing test-hook` has something to call.
func (w *Webhook) Deliver(ctx context.Context, event string, payload map[string]any) error {
	if !w.Enabled() {
		return nil
	}

	delivery := uuid.NewString()
	body, err := json.Marshal(map[string]any{
		"event":       event,
		"delivery_id": delivery,
		"sent_at":     w.now().UTC().Format(time.RFC3339),
		"data":        payload,
	})
	if err != nil {
		// A payload that will not marshal will not marshal on the retry either.
		return fmt.Errorf("encode %s event: %w", event, err)
	}

	var lastErr error
	for attempt := 0; attempt < w.attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: giving up after %d attempt(s): %w", event, attempt, ctx.Err())
			case <-time.After(w.backoff(attempt - 1)):
			}
		}

		retry, err := w.attempt(ctx, event, delivery, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			// A 4xx is the receiver saying this request is wrong. Repeating it
			// unchanged cannot make it right, and hammering an endpoint that has
			// already refused is how a best-effort hook becomes an outage.
			return fmt.Errorf("%s: refused permanently: %w", event, err)
		}
	}
	return fmt.Errorf("%s: %d attempt(s) failed: %w", event, w.attempts, lastErr)
}

// attempt makes one request, reporting whether the failure is worth retrying.
func (w *Webhook) attempt(ctx context.Context, event, delivery string, body []byte) (retry bool, err error) {
	ts := strconv.FormatInt(w.now().UTC().Unix(), 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "private-cloud-billing-hook")
	req.Header.Set(EventHeader, event)
	req.Header.Set(DeliveryHeader, delivery)
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, "v1="+Sign(w.secret, ts, body))

	resp, err := w.client.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout: transient by assumption,
		// which is the assumption a retry exists to make.
		return true, err
	}
	defer func() {
		// Drained before closing so the connection can be reused; bounded so a
		// receiver answering with a firehose cannot be a memory problem here.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		// 429 and 5xx are the receiver saying "not now", which is the one thing
		// a retry answers.
		return true, fmt.Errorf("receiver returned %d", resp.StatusCode)
	default:
		return false, fmt.Errorf("receiver returned %d", resp.StatusCode)
	}
}

// Sign computes the hex HMAC-SHA256 over "<timestamp>.<body>".
//
// The timestamp is inside the signed material rather than beside it, which is
// what makes the signature bound to a moment: a receiver that rejects a stale
// timestamp is then rejecting a replay, because moving the timestamp forward
// invalidates the MAC. Signing the body alone would let an intercepted delivery
// be replayed indefinitely and still verify.
//
// The whole construction is stdlib. A signing scheme is exactly the wrong place
// to add a dependency: it has to be readable by whoever implements the
// verifying half, and "it does whatever that library does" is not a spec.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature the way a receiver should, and exists so the
// documented verification is executable rather than prose.
//
// hmac.Equal, not ==: a byte-at-a-time comparison leaks where two MACs first
// differ, which is enough to forge one a byte at a time.
func Verify(secret []byte, timestamp string, body []byte, signature string) bool {
	want, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	got, err := hex.DecodeString(Sign(secret, timestamp, body))
	if err != nil {
		return false
	}
	return hmac.Equal(want, got)
}
