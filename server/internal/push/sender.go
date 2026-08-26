package push

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrGone means the subscription is dead and should be deleted.
//
// It is worth a distinct error because it is the one push failure that is not
// transient and not ignorable: the browser told its push service to forget this
// subscription — the user cleared site data, uninstalled the PWA, or the service
// expired it. Retrying is pointless and keeping the row means trying forever, so
// the caller unregisters instead.
var ErrGone = errors.New("push subscription is gone")

// sendTimeout bounds one delivery. Short on purpose: this runs off the back of
// something a user is waiting on, and a push service that is slow must not
// become this server being slow. The notification is an optimisation; the
// deadline protects the thing it was optimising.
const sendTimeout = 10 * time.Second

// defaultTTL is how long the push service should hold an undelivered message.
// Four hours: long enough to cover a phone that is asleep or out of signal for
// an evening, short enough that a notification about a file change does not
// arrive stale the next day. A client that missed it learns from GET /changes
// anyway, which is precisely why this can be finite without losing anything.
const defaultTTL = 4 * time.Hour

// Sender delivers notifications. One per process; safe for concurrent use.
type Sender struct {
	keys   *Keys
	client *http.Client
}

// NewSender builds a sender around a VAPID identity.
func NewSender(keys *Keys) *Sender {
	return &Sender{
		keys: keys,
		// A dedicated client rather than http.DefaultClient: this talks to
		// third-party services on the public internet, and it should not share
		// connection state or timeouts with anything else the process does.
		client: &http.Client{Timeout: sendTimeout},
	}
}

// PublicKey is the applicationServerKey a browser needs before it can subscribe.
func (s *Sender) PublicKey() string { return s.keys.Public }

// Send encrypts one payload to one subscription and posts it.
//
// The payload is opaque here — the caller decides what a notification says, and
// what it says is deliberately thin; see the notify package's comment on why a
// push carries a hint rather than content.
func (s *Sender) Send(ctx context.Context, sub Subscription, payload []byte) error {
	body, err := encrypt(sub, payload)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}

	auth, err := s.keys.authorization(sub.Endpoint, time.Now())
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", fmt.Sprintf("%d", int(defaultTTL.Seconds())))
	// Without this a service is free to deliver whenever it next wakes the
	// device, which for a change notification is too late to be the latency win
	// it exists to be.
	req.Header.Set("Urgency", "normal")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("post to push service: %w", err)
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused; bounded because the body is an
	// error message from a third party and is not otherwise interesting.
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))

	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	default:
		return fmt.Errorf("push service returned %d: %s", resp.StatusCode, bytes.TrimSpace(detail))
	}
}
