package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The device endpoints, end to end over the real router.
//
// The property worth most here is not the listing — it is that a revoked device
// stops working on its very next request. Revocation that takes effect "soon" is
// not an answer to a lost laptop.

// deviceToken mints an app password and exchanges it for a device token, with a
// chosen User-Agent so the advisory platform/version fields have something to
// parse.
func (f *apiFixture) deviceToken(agent string) string {
	f.t.Helper()

	rec := f.json(http.MethodPost, "/api/v1/auth/app-passwords", map[string]any{"name": "test-device"})
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create app password = %d: %s", rec.Code, rec.Body)
	}
	appPassword := decode(f.t, rec)["password"].(string)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", nil)
	req.Header.Set("Authorization", basicAuth(f.username, appPassword))
	req.Header.Set("User-Agent", agent)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("token exchange = %d: %s", rec.Code, rec.Body)
	}
	return decode(f.t, rec)["token"].(string)
}

// devices reads the caller's device list.
func (f *apiFixture) devices() []any {
	f.t.Helper()
	rec := f.json(http.MethodGet, "/api/v1/devices", nil)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("list devices = %d: %s", rec.Code, rec.Body)
	}
	list, _ := decode(f.t, rec)["devices"].([]any)
	return list
}

func (f *apiFixture) firstDeviceID() string {
	f.t.Helper()
	list := f.devices()
	if len(list) == 0 {
		f.t.Fatal("no devices listed")
	}
	return list[0].(map[string]any)["id"].(string)
}

func TestDeviceListReportsAdvisoryClientIdentity(t *testing.T) {
	f := newAPIFixture(t)
	f.deviceToken("pcsync/0.4.1 (linux)")

	list := f.devices()
	if len(list) != 1 {
		t.Fatalf("got %d device(s), want 1", len(list))
	}
	d := list[0].(map[string]any)

	if d["platform"] != "linux" {
		t.Errorf("platform = %v, want linux", d["platform"])
	}
	if d["app_version"] != "0.4.1" {
		t.Errorf("app_version = %v, want 0.4.1", d["app_version"])
	}
	if d["has_push"] != false {
		t.Errorf("has_push = %v, want false before any registration", d["has_push"])
	}
	// Listed from the browser session, so the device is not the caller. `current`
	// exists precisely so a UI need not infer this from ids.
	if d["current"] != false {
		t.Errorf("current = %v — a browser session is not the device it lists", d["current"])
	}
}

func TestDeviceRenameSticks(t *testing.T) {
	f := newAPIFixture(t)
	f.deviceToken("Go-http-client/2.0")
	id := f.firstDeviceID()

	rec := f.json(http.MethodPatch, "/api/v1/devices/"+id, map[string]any{"name": "the laptop"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body)
	}

	if got := f.devices()[0].(map[string]any)["name"]; got != "the laptop" {
		t.Errorf("name = %v, want the laptop", got)
	}
}

// An agent that identifies nothing still gets a usable name rather than a blank
// row — "unknown device" is the string the contract says a person should be able
// to fix.
func TestUnidentifiableDeviceGetsAFallbackName(t *testing.T) {
	f := newAPIFixture(t)
	f.deviceToken("????")

	if got := f.devices()[0].(map[string]any)["name"]; got != "unknown device" {
		t.Errorf("name = %v, want unknown device", got)
	}
}

// The one that matters.
func TestRevokingADeviceKillsItsTokenImmediately(t *testing.T) {
	f := newAPIFixture(t)
	token := f.deviceToken("pcsync/0.4.1 (linux)")

	if rec := f.bearer(http.MethodGet, "/api/v1/auth/me", token); rec.Code != http.StatusOK {
		t.Fatalf("device token rejected before revocation: %d", rec.Code)
	}

	id := f.firstDeviceID()
	if rec := f.json(http.MethodDelete, "/api/v1/devices/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body)
	}

	if rec := f.bearer(http.MethodGet, "/api/v1/auth/me", token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d — revocation must be immediate", rec.Code)
	}
	// And it drops out of the list rather than lingering as a dead row.
	if len(f.devices()) != 0 {
		t.Errorf("revoked device is still listed")
	}
}

// A DEVICE session must not reach the device plane at all.
//
// DELETE /devices/{id} revokes a token and PATCH renames one, which is
// credential management wearing a different path prefix. Without this, one
// leaked app password could revoke every other device on the account — the same
// escalation the /auth/ confinement already prevents.
func TestDeviceSessionCannotManageDevices(t *testing.T) {
	f := newAPIFixture(t)
	token := f.deviceToken("pcsync/0.4.1 (linux)")
	id := f.firstDeviceID()

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/devices"},
		{http.MethodPatch, "/api/v1/devices/" + id},
		{http.MethodDelete, "/api/v1/devices/" + id},
		{http.MethodPost, "/api/v1/devices/" + id + "/push"},
		{http.MethodDelete, "/api/v1/devices/" + id + "/push"},
	} {
		rec := f.bearer(c.method, c.path, token)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s from a device token = %d, want 403", c.method, c.path, rec.Code)
		}
	}

	// The file and sync planes still work — the confinement is about credentials,
	// not about crippling the client.
	if rec := f.bearer(http.MethodGet, "/api/v1/nodes/root", token); rec.Code != http.StatusOK {
		t.Errorf("device token lost access to the file plane: %d", rec.Code)
	}
}

// A device belongs to one account, and the answer must not distinguish "not
// yours" from "no such device".
func TestAnotherUsersDeviceIsNotReachable(t *testing.T) {
	f := newAPIFixture(t)
	f.deviceToken("pcsync/0.4.1 (linux)")
	id := f.firstDeviceID()

	// f.admin is a different account entirely.
	if rec := f.do(http.MethodDelete, "/api/v1/devices/"+id, nil, f.admin); rec.Code != http.StatusNotFound {
		t.Errorf("another user's revoke = %d, want 404", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/api/v1/devices", nil, f.admin); rec.Code == http.StatusOK {
		if list, _ := decode(t, rec)["devices"].([]any); len(list) != 0 {
			t.Errorf("another user's device list is not empty: %d entries", len(list))
		}
	}
}

func TestPushSubscriptionRoundTrip(t *testing.T) {
	f := newAPIFixture(t)
	f.deviceToken("pcsync/0.4.1 (linux)")
	id := f.firstDeviceID()

	sub := map[string]any{
		"endpoint": "https://push.example.com/abc",
		"keys":     map[string]any{"p256dh": "key", "auth": "secret"},
	}
	if rec := f.json(http.MethodPost, "/api/v1/devices/"+id+"/push", sub); rec.Code != http.StatusOK {
		t.Fatalf("register push = %d: %s", rec.Code, rec.Body)
	}
	if f.devices()[0].(map[string]any)["has_push"] != true {
		t.Error("has_push is false after registering a subscription")
	}

	// Re-registering replaces rather than conflicting: a browser hands out a new
	// endpoint whenever its subscription is refreshed.
	sub["endpoint"] = "https://push.example.com/def"
	if rec := f.json(http.MethodPost, "/api/v1/devices/"+id+"/push", sub); rec.Code != http.StatusOK {
		t.Fatalf("re-register push = %d: %s", rec.Code, rec.Body)
	}

	if rec := f.json(http.MethodDelete, "/api/v1/devices/"+id+"/push", nil); rec.Code != http.StatusOK {
		t.Fatalf("unregister = %d: %s", rec.Code, rec.Body)
	}
	if f.devices()[0].(map[string]any)["has_push"] != false {
		t.Error("has_push is still true after unregistering")
	}
}

// The endpoint is opaque to us — it belongs to a push service we never talk to —
// so validation stops at "absolute https URL with both keys present".
func TestPushRejectsAnUnusableSubscription(t *testing.T) {
	f := newAPIFixture(t)
	f.deviceToken("pcsync/0.4.1 (linux)")
	id := f.firstDeviceID()

	for _, bad := range []map[string]any{
		{"endpoint": "", "keys": map[string]any{"p256dh": "k", "auth": "a"}},
		{"endpoint": "http://push.example.com/abc", "keys": map[string]any{"p256dh": "k", "auth": "a"}},
		{"endpoint": "https://push.example.com/abc", "keys": map[string]any{"p256dh": "", "auth": "a"}},
		{"endpoint": "https://push.example.com/abc", "keys": map[string]any{"p256dh": "k", "auth": ""}},
	} {
		if rec := f.json(http.MethodPost, "/api/v1/devices/"+id+"/push", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("subscription %v got %d, want 400", bad, rec.Code)
		}
	}
}

// Revoking a device takes its push registration with it: a revoked device must
// stop being a delivery target in the same instant it stops being able to read.
func TestRevokingADeviceRemovesItsPushSubscription(t *testing.T) {
	f := newAPIFixture(t)
	f.deviceToken("pcsync/0.4.1 (linux)")
	id := f.firstDeviceID()

	sub := map[string]any{
		"endpoint": "https://push.example.com/abc",
		"keys":     map[string]any{"p256dh": "key", "auth": "secret"},
	}
	if rec := f.json(http.MethodPost, "/api/v1/devices/"+id+"/push", sub); rec.Code != http.StatusOK {
		t.Fatalf("register push = %d", rec.Code)
	}
	if rec := f.json(http.MethodDelete, "/api/v1/devices/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}

	var count int
	err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM push_subscriptions WHERE session_id = $1`, id).Scan(&count)
	if err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if count != 0 {
		t.Errorf("%d push subscription(s) survived revocation", count)
	}
}
