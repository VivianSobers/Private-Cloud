package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
)

// Phase 6: the device list.
//
// GET /auth/sessions already returns these same rows, and this endpoint exists
// anyway because the two answer different questions. A session list is "what is
// currently authenticated"; a device list is "which of my machines is syncing",
// which wants the client's identity and a name a person chose.

func deviceJSON(d *auth.Device, current bool) map[string]any {
	out := map[string]any{
		"id":           d.ID,
		"name":         d.Name,
		"last_seen_at": d.LastSeenAt,
		"created_at":   d.CreatedAt,
		"expires_at":   d.ExpiresAt,
		"has_push":     d.HasPush,
		// Marks the caller's own device so a UI can avoid offering "revoke" on
		// the session doing the revoking, without having to infer it from ids.
		"current": current,
	}
	// Advisory and self-reported: omitted rather than sent empty, so a client
	// cannot mistake "the agent told us nothing" for "the platform is blank".
	if d.Platform != "" {
		out["platform"] = d.Platform
	}
	if d.AppVersion != "" {
		out["app_version"] = d.AppVersion
	}
	return out
}

func (s *Server) writeDeviceError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, auth.ErrDeviceNotFound) {
		writeError(w, r, http.StatusNotFound, "not_found", "no such device")
		return
	}
	s.serverError(w, r, op, err)
}

func (s *Server) deviceIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid device id")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.auth.Store().ListDevices(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "list devices", err)
		return
	}

	// The caller's own session, when it is itself a device token — a browser
	// session driving this endpoint is not in the list at all, and marks nothing.
	var currentID uuid.UUID
	if sess := currentSession(r.Context()); sess != nil {
		currentID = sess.ID
	}

	out := make([]map[string]any, 0, len(devices))
	for _, d := range devices {
		out = append(out, deviceJSON(d, d.ID == currentID))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"devices": out})
}

func (s *Server) handlePatchDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := s.deviceIDParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_name", "a device needs a name")
		return
	}

	if err := s.auth.Store().RenameDevice(r.Context(), CurrentUser(r.Context()).ID, id, body.Name); err != nil {
		s.writeDeviceError(w, r, "rename device", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "renamed"})
}

// handleRevokeDevice kills a device's token. The client gets 401 on its next
// call — the token is checked per request and never cached, which is what makes
// this a real answer to a lost laptop rather than an eventual one.
func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := s.deviceIDParam(w, r)
	if !ok {
		return
	}
	user := CurrentUser(r.Context())

	if err := s.auth.Store().RevokeDevice(r.Context(), user.ID, id); err != nil {
		s.writeDeviceError(w, r, "revoke device", err)
		return
	}
	s.log.Info("device revoked", "user", user.Username, "device", id,
		"request_id", RequestID(r.Context()))
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "revoked"})
}

// handleRegisterPush stores a Web Push subscription for a device.
//
// A hook, not a service: nothing here contacts a push provider. The server holds
// what a client registered so that something else can deliver, and a client that
// registers nothing keeps polling GET /changes. Any client must work with this
// switched off.
func (s *Server) handleRegisterPush(w http.ResponseWriter, r *http.Request) {
	id, ok := s.deviceIDParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	body.Endpoint = strings.TrimSpace(body.Endpoint)
	if body.Endpoint == "" || body.Keys.P256dh == "" || body.Keys.Auth == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request",
			"endpoint and both keys are required")
		return
	}
	// Only that it is an absolute HTTPS URL. The value is otherwise opaque — it
	// belongs to the browser's push service, not to us — and parsing it further
	// would be inventing a contract with a party we never talk to.
	if !strings.HasPrefix(body.Endpoint, "https://") {
		writeError(w, r, http.StatusBadRequest, "invalid_request",
			"the push endpoint must be an https URL")
		return
	}

	err := s.auth.Store().PutPushSubscription(r.Context(), CurrentUser(r.Context()).ID, id,
		body.Endpoint, body.Keys.P256dh, body.Keys.Auth)
	if err != nil {
		s.writeDeviceError(w, r, "register push", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "registered"})
}

func (s *Server) handleUnregisterPush(w http.ResponseWriter, r *http.Request) {
	id, ok := s.deviceIDParam(w, r)
	if !ok {
		return
	}
	if err := s.auth.Store().DeletePushSubscription(r.Context(), CurrentUser(r.Context()).ID, id); err != nil {
		s.writeDeviceError(w, r, "unregister push", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "unregistered"})
}
