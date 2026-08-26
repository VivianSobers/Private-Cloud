package push

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Notifier fans one event out to every live device a user has registered.
//
// What a notification SAYS is the design decision here, and it says almost
// nothing: a type and a change cursor. No filename, no path, no preview.
//
// Two reasons, and the second is the one that settles it. The first is that the
// client already knows how to find out what changed — it calls GET /changes with
// the cursor, over the tailnet, authenticated — so putting content in the push
// would duplicate a mechanism that already exists and has to keep existing for
// clients that never subscribed. The second is that a push travels through a
// browser vendor's infrastructure. The payload is encrypted end to end, so they
// cannot read it, but its SIZE and TIMING are still theirs to see, and the less
// the plaintext varies with the content the less those leak. A system whose
// premise is that your files do not leave your infrastructure should not make an
// exception for a convenience feature.
type Notifier struct {
	sender *Sender
	store  TargetStore
	seqs   SeqSource
	log    *slog.Logger

	// fanoutTimeout bounds the whole fan-out, not one delivery, so a user with
	// many devices and one unreachable vendor cannot hold a goroutine open
	// indefinitely.
	fanoutTimeout time.Duration

	// lastNotified is the highest cursor each user has already been told about.
	//
	// It is what makes the trigger safe to call indiscriminately: the caller
	// says "this user may have changes" after any successful write, and the
	// journal decides whether anything actually moved. A request that changed
	// nothing — a no-op rename, a re-PUT of identical bytes, a metadata read
	// dressed as a POST — leaves the cursor where it was and sends nothing.
	//
	// In-memory, and losing it costs one redundant notification per user after
	// a restart, which is the correct trade against persisting a value whose
	// only job is suppressing a duplicate.
	mu           sync.Mutex
	lastNotified map[uuid.UUID]int64
}

// SeqSource reads the journal cursor. Narrow for the same reason TargetStore is.
type SeqSource interface {
	LatestSeq(ctx context.Context, ownerID uuid.UUID) (int64, error)
}

// TargetStore is the slice of the auth store this package needs. Narrow on
// purpose: a notifier that could reach the rest of the session table would be a
// notifier that could revoke one.
type TargetStore interface {
	PushTargetsFor(ctx context.Context, userID uuid.UUID) ([]Target, error)
	DeletePushSubscriptionBySession(ctx context.Context, sessionID uuid.UUID) error
}

// Target is one device's subscription.
type Target struct {
	SessionID uuid.UUID
	Endpoint  string
	P256dh    string
	Auth      string
}

// NewNotifier builds a notifier. A nil sender yields a nil notifier, and every
// method on a nil notifier is a no-op — so "push is not configured" needs no
// branch at any call site.
func NewNotifier(sender *Sender, store TargetStore, seqs SeqSource, log *slog.Logger) *Notifier {
	if sender == nil {
		return nil
	}
	return &Notifier{
		sender:        sender,
		store:         store,
		seqs:          seqs,
		log:           log,
		fanoutTimeout: 30 * time.Second,
		lastNotified:  map[uuid.UUID]int64{},
	}
}

// ChangePayload is what a "something changed" notification carries.
type ChangePayload struct {
	// Type lets a service worker branch without guessing from shape.
	Type string `json:"type"`
	// Seq is the journal cursor the client should catch up to. It is a number,
	// not a description: the client turns it into detail by asking the API.
	Seq int64 `json:"seq"`
}

// NotifyChange tells a user's devices that their journal has advanced.
//
// Fire and forget, deliberately. The caller is on a request path that has
// already succeeded — the file is written, the journal row is committed — and a
// push service being slow or down must not turn a successful upload into a slow
// one or a failed one. Nothing here is retried: the client polls, so the worst
// case of a dropped notification is that a device notices a few seconds later,
// which is the situation every client was in before push existed.
func (n *Notifier) NotifyChange(userID uuid.UUID) {
	if n == nil {
		return
	}
	go n.notify(userID)
}

// notify reads the cursor, decides whether anything moved, and fans out.
//
// The read happens here rather than at the call site so the request path pays
// nothing for a feature that is an optimisation: a handler calls NotifyChange
// and returns, and the query, the comparison and the delivery all happen after
// the response has gone.
func (n *Notifier) notify(userID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), n.fanoutTimeout)
	defer cancel()

	seq, err := n.seqs.LatestSeq(ctx, userID)
	if err != nil {
		n.log.Warn("push: read change cursor", "user", userID, "error", err)
		return
	}

	n.mu.Lock()
	moved := seq > n.lastNotified[userID]
	if moved {
		n.lastNotified[userID] = seq
	}
	n.mu.Unlock()
	if !moved {
		return
	}

	payload, err := json.Marshal(ChangePayload{Type: "changes", Seq: seq})
	if err != nil {
		n.log.Warn("push payload", "error", err)
		return
	}
	n.fanout(ctx, userID, payload)
}

// fanout delivers to every target concurrently.
//
// The context is detached from whatever request triggered this: the request is
// finishing now, and cancelling delivery because the HTTP handler returned would
// mean the notification is never sent in the common case rather than the rare
// one.
func (n *Notifier) fanout(ctx context.Context, userID uuid.UUID, payload []byte) {
	targets, err := n.store.PushTargetsFor(ctx, userID)
	if err != nil {
		n.log.Warn("push targets", "user", userID, "error", err)
		return
	}

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			err := n.sender.Send(ctx, Subscription{
				Endpoint: t.Endpoint, P256dh: t.P256dh, Auth: t.Auth,
			}, payload)

			switch {
			case err == nil:
			case errors.Is(err, ErrGone):
				// The browser told its push service to forget this
				// subscription. Keeping the row means trying forever, so this
				// is the one push failure worth acting on.
				if err := n.store.DeletePushSubscriptionBySession(ctx, t.SessionID); err != nil {
					n.log.Warn("unregister dead push subscription", "session", t.SessionID, "error", err)
				} else {
					n.log.Info("unregistered a push subscription the service reported gone",
						"session", t.SessionID)
				}
			default:
				n.log.Warn("push delivery failed", "session", t.SessionID, "error", err)
			}
		}(t)
	}
	wg.Wait()
}
