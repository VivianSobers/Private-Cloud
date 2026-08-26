package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/push"
)

// pushTargets adapts the auth store to the narrow interface the notifier wants,
// the same way the extract and media handlers are adapted to the files store.
// The translation is one struct copy and it buys the push package independence
// from the session schema.
type pushTargets struct{ store *auth.Store }

func (p pushTargets) PushTargetsFor(ctx context.Context, userID uuid.UUID) ([]push.Target, error) {
	rows, err := p.store.PushTargetsFor(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]push.Target, len(rows))
	for i, r := range rows {
		out[i] = push.Target{SessionID: r.SessionID, Endpoint: r.Endpoint, P256dh: r.P256dh, Auth: r.Auth}
	}
	return out, nil
}

func (p pushTargets) DeletePushSubscriptionBySession(ctx context.Context, sessionID uuid.UUID) error {
	return p.store.DeletePushSubscriptionBySession(ctx, sessionID)
}

// notifyIfChanged fires a push after a request that may have advanced the
// caller's change journal.
//
// Called from requireAuth rather than from each mutating handler, and that is
// the whole point: "which handlers write" is a list, and a list is a thing that
// goes stale the first time somebody adds a route and does not think about push.
// requireAuth is the one place every authenticated request already passes
// through, so a write added tomorrow notifies without anyone remembering.
//
// Firing indiscriminately is safe because the notifier asks the journal whether
// the cursor actually moved and stays quiet when it did not — so a GET-shaped
// POST, a no-op rename, or a re-upload of identical bytes sends nothing. The
// alternative, deciding here, would mean re-deriving "did this write anything"
// from the HTTP method, which is exactly the guess the journal already answers.
func (s *Server) notifyIfChanged(r *http.Request, status int, userID uuid.UUID) {
	if s.notifier == nil {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return
	}
	if status < 200 || status >= 300 {
		return
	}
	s.notifier.NotifyChange(userID)
}

// handlePushKey publishes the VAPID application server key.
//
// This is the endpoint whose absence made `POST /devices/{id}/push` the only
// route in the repository served with no client: PushManager.subscribe cannot be
// called without it, so no browser could ever produce a subscription to register.
//
// Authenticated, because everything under /api/v1 is, though the value is not
// secret — it is a public key, and it is handed to every browser that subscribes.
//
// Answering 404 when push is unconfigured rather than 200 with an empty key is
// the same convention OIDC uses: a client feature-detects by asking, and an empty
// string would have to be special-cased by every caller into the "not available"
// it already means.
func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		writeError(w, r, http.StatusNotFound, "push_disabled",
			"this server has no VAPID key configured, so it cannot send push notifications")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"public_key": s.push.PublicKey()})
}
