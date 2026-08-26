package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Devices (Phase 6).
//
// A device is a session of kind 'device'. GET /auth/sessions already returns
// those rows, but a device list wants the CLIENT's identity — what it is, what
// version it runs, when it last checked in — rather than the session's.

// Device is one registered client.
type Device struct {
	ID   uuid.UUID
	Name string
	// Platform and AppVersion are parsed from the User-Agent the client sent when
	// it exchanged its app password. Self-reported and therefore ADVISORY ONLY:
	// they are for a person recognising their own laptop in a list, and must
	// never gate anything.
	Platform   string
	AppVersion string
	UserAgent  string
	LastSeenAt time.Time
	CreatedAt  time.Time
	ExpiresAt  time.Time
	// HasPush reports whether this device registered a Web Push subscription. A
	// device without one polls GET /changes, which is the working path.
	HasPush bool
}

// ErrDeviceNotFound means no live device session with that id belongs to the
// caller. Indistinguishable from "already revoked" on purpose.
var ErrDeviceNotFound = errors.New("device not found")

// ListDevices returns the user's live device sessions, most recently seen first.
func (s *Store) ListDevices(ctx context.Context, userID uuid.UUID) ([]*Device, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.device_name, s.user_agent, s.last_seen_at, s.created_at, s.expires_at,
		       (p.session_id IS NOT NULL)
		FROM sessions s
		LEFT JOIN push_subscriptions p ON p.session_id = s.id
		WHERE s.user_id = $1
		  AND s.kind = $2
		  AND s.revoked_at IS NULL
		  AND s.expires_at > now()
		ORDER BY s.last_seen_at DESC`, userID, SessionKindDevice)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Device, 0, 8)
	for rows.Next() {
		d := &Device{}
		if err := rows.Scan(&d.ID, &d.Name, &d.UserAgent, &d.LastSeenAt,
			&d.CreatedAt, &d.ExpiresAt, &d.HasPush); err != nil {
			return nil, err
		}
		d.Platform, d.AppVersion = ParseUserAgent(d.UserAgent)
		if d.Name == "" {
			d.Name = defaultDeviceName(d.Platform, d.UserAgent)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RenameDevice sets the human name for one of the caller's devices.
func (s *Store) RenameDevice(ctx context.Context, userID, deviceID uuid.UUID, name string) error {
	name = strings.TrimSpace(name)
	if len(name) > 128 {
		name = name[:128]
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions SET device_name = $3
		WHERE id = $1 AND user_id = $2 AND kind = $4 AND revoked_at IS NULL`,
		deviceID, userID, name, SessionKindDevice)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// RevokeDevice kills a device's token.
//
// Scoped to kind='device' rather than reusing RevokeSession so that a client
// driving the device list cannot revoke the browser session it is being driven
// from by passing its id — the two are managed by different screens for
// different reasons, and conflating them makes "log out my laptop" able to log
// out the tab you clicked it in.
//
// Revocation is immediate: the token is checked per request and never cached,
// which is the property that makes "I lost my laptop" a real answer.
//
// It also drops the device's push subscription, in the SAME statement. The
// foreign key cascade does not cover this: revoking a session is a soft delete
// that stamps revoked_at, so no row is ever removed and ON DELETE CASCADE never
// fires. Without the explicit delete a revoked device stays a delivery target —
// still receiving notifications about a library it can no longer read, which is
// the one thing "I lost my laptop" most needs not to happen.
//
// One statement rather than two, so a failure between them cannot leave a live
// subscription against a dead session. The count comes from the UPDATE, not the
// DELETE, because a device that never registered for push must still revoke.
func (s *Store) RevokeDevice(ctx context.Context, userID, deviceID uuid.UUID) error {
	var revoked int
	err := s.pool.QueryRow(ctx, `
		WITH revoked AS (
			UPDATE sessions SET revoked_at = now()
			WHERE id = $1 AND user_id = $2 AND kind = $3 AND revoked_at IS NULL
			RETURNING id
		), dropped AS (
			DELETE FROM push_subscriptions WHERE session_id IN (SELECT id FROM revoked)
		)
		SELECT count(*) FROM revoked`,
		deviceID, userID, SessionKindDevice).Scan(&revoked)
	if err != nil {
		return err
	}
	if revoked == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// PutPushSubscription registers or replaces a device's Web Push endpoint.
//
// Verifies the session is the caller's own live device in the same statement as
// the insert: a subscription is a delivery target, and one attached to a session
// the caller does not own would send that user's notifications elsewhere.
func (s *Store) PutPushSubscription(ctx context.Context, userID, deviceID uuid.UUID, endpoint, p256dh, authKey string) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO push_subscriptions (session_id, user_id, endpoint, p256dh, auth_key)
		SELECT s.id, s.user_id, $3, $4, $5
		FROM sessions s
		WHERE s.id = $1 AND s.user_id = $2 AND s.kind = $6
		  AND s.revoked_at IS NULL AND s.expires_at > now()
		ON CONFLICT (session_id) DO UPDATE SET
			endpoint = excluded.endpoint,
			p256dh = excluded.p256dh,
			auth_key = excluded.auth_key,
			updated_at = now()`,
		deviceID, userID, endpoint, p256dh, authKey, SessionKindDevice)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// PushTarget is one deliverable subscription. SessionID identifies which device
// it belongs to, so a delivery that comes back "gone" can be unregistered
// precisely rather than clearing the user's other devices with it.
type PushTarget struct {
	SessionID uuid.UUID
	Endpoint  string
	P256dh    string
	Auth      string
}

// PushTargetsFor lists a user's live push subscriptions.
//
// Joined against sessions rather than read from push_subscriptions alone, and
// that join is the point: revoking a session is a SOFT delete that stamps
// revoked_at and removes no row, so ON DELETE CASCADE never fires and a revoked
// device's subscription outlives the device. Reading the table on its own would
// keep notifying a laptop whose access was withdrawn — about a library it can no
// longer open. The same bug was found and fixed once already for the delete
// path; this is the read path having learned it.
func (s *Store) PushTargetsFor(ctx context.Context, userID uuid.UUID) ([]PushTarget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.session_id, p.endpoint, p.p256dh, p.auth_key
		FROM push_subscriptions p
		JOIN sessions s ON s.id = p.session_id
		WHERE p.user_id = $1
		  AND s.revoked_at IS NULL AND s.expires_at > now()`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PushTarget
	for rows.Next() {
		var t PushTarget
		if err := rows.Scan(&t.SessionID, &t.Endpoint, &t.P256dh, &t.Auth); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeletePushSubscriptionBySession removes one dead subscription, without
// requiring the caller to prove ownership again — it is only reached after the
// push service reported the endpoint gone for a row already scoped to a user.
func (s *Store) DeletePushSubscriptionBySession(ctx context.Context, sessionID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE session_id = $1`, sessionID)
	return err
}

// DeletePushSubscription unregisters a device's push endpoint. The device keeps
// working; it falls back to polling GET /changes.
func (s *Store) DeletePushSubscription(ctx context.Context, userID, deviceID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM push_subscriptions WHERE session_id = $1 AND user_id = $2`, deviceID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// uaPattern matches the "name/version" products a well-behaved client sends,
// e.g. "pcsync/0.4.1 (linux)".
var uaPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+)/([A-Za-z0-9_.+-]+)`)

// ParseUserAgent extracts an advisory platform and version.
//
// Best effort by design. Everything it returns is self-reported by the client,
// so it exists to help a person recognise their own laptop in a list and for
// nothing else. An unrecognised agent yields empty strings rather than a guess:
// a wrong platform is worse than none, because it makes the list look
// authoritative when it is not.
func ParseUserAgent(ua string) (platform, version string) {
	if ua == "" {
		return "", ""
	}
	if m := uaPattern.FindStringSubmatch(ua); m != nil {
		version = m[2]
	}

	lower := strings.ToLower(ua)
	switch {
	case strings.Contains(lower, "windows"):
		platform = "windows"
	case strings.Contains(lower, "android"):
		// Checked before "linux": Android user agents contain both, and the more
		// specific answer is the useful one.
		platform = "android"
	case strings.Contains(lower, "iphone"), strings.Contains(lower, "ipad"), strings.Contains(lower, "ios"):
		platform = "ios"
	case strings.Contains(lower, "mac os"), strings.Contains(lower, "macos"), strings.Contains(lower, "darwin"):
		platform = "macos"
	case strings.Contains(lower, "linux"):
		platform = "linux"
	}
	return platform, version
}

// defaultDeviceName is what a device is called before anyone renames it.
// "unknown device" is the string the contract says a user should be able to fix,
// so it is only reached when the agent tells us nothing at all.
func defaultDeviceName(platform, ua string) string {
	if platform != "" {
		return platform + " device"
	}
	if m := uaPattern.FindStringSubmatch(ua); m != nil {
		return m[1]
	}
	return "unknown device"
}
