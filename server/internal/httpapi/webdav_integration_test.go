package httpapi_test

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// WebDAV end-to-end. The point of this surface is that Finder, Explorer and
// rclone can mount it, so the tests exercise the request shapes those clients
// actually send — PROPFIND with a Depth header, MKCOL, MOVE with a Destination,
// and Basic auth with an app password.

// multistatus is the minimum of RFC 4918's response body needed to assert on a
// directory listing.
type multistatus struct {
	XMLName   xml.Name `xml:"multistatus"`
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Prop struct {
				DisplayName   string `xml:"displayname"`
				ContentLength string `xml:"getcontentlength"`
				ResourceType  struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
			} `xml:"prop"`
			Status string `xml:"status"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// appPassword mints a credential for the fixture's user.
func (f *apiFixture) appPassword(name string) string {
	f.t.Helper()
	rec := f.json(http.MethodPost, "/api/v1/auth/app-passwords", map[string]any{"name": name})
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create app password = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		f.t.Fatal(err)
	}
	if body.Password == "" {
		f.t.Fatal("app password response carried no password")
	}
	return body.Password
}

// dav issues a WebDAV request authenticated with an app password.
func (f *apiFixture) dav(method, path, password, body string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	var r *strings.Reader
	if body == "" {
		r = strings.NewReader("")
	} else {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.SetBasicAuth(f.username, password)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestDavRequiresCredentials(t *testing.T) {
	f := newAPIFixture(t)

	req := httptest.NewRequest("PROPFIND", "/dav/", nil)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PROPFIND = %d, want 401", rec.Code)
	}
	// Without a challenge, a mounting client has no idea what to ask for.
	if auth := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(auth, "Basic ") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", auth)
	}
}

func TestDavRejectsBadAppPassword(t *testing.T) {
	f := newAPIFixture(t)
	good := f.appPassword("test client")

	for _, bad := range []string{"", "wrong", "pcap_short", good + "x", strings.Replace(good, "pcap_", "xxxx_", 1)} {
		rec := f.dav("PROPFIND", "/dav/", bad, "", map[string]string{"Depth": "0"})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("PROPFIND with %q = %d, want 401", bad, rec.Code)
		}
	}
}

func TestDavFullLifecycle(t *testing.T) {
	f := newAPIFixture(t)
	pw := f.appPassword("integration client")

	// MKCOL — "New Folder" in Finder.
	if rec := f.dav("MKCOL", "/dav/Documents", pw, "", nil); rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL = %d: %s", rec.Code, rec.Body)
	}

	// PUT — dragging a file in.
	const content = "hello from a network drive"
	rec := f.dav("PUT", "/dav/Documents/note.txt", pw, content, nil)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}

	// GET — opening it.
	rec = f.dav("GET", "/dav/Documents/note.txt", pw, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	if rec.Body.String() != content {
		t.Errorf("GET body = %q, want %q", rec.Body.String(), content)
	}
	// A strong validator, so a mount does not re-download unchanged files.
	if rec.Header().Get("ETag") == "" {
		t.Error("no ETag on a WebDAV GET")
	}

	// PROPFIND Depth:1 — a directory listing.
	rec = f.dav("PROPFIND", "/dav/Documents", pw, "", map[string]string{"Depth": "1"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND = %d, want 207: %s", rec.Code, rec.Body)
	}
	var ms multistatus
	if err := xml.Unmarshal(rec.Body.Bytes(), &ms); err != nil {
		t.Fatalf("PROPFIND body is not valid multistatus XML: %v\n%s", err, rec.Body)
	}
	// Depth 1 returns the collection itself plus its children.
	if len(ms.Responses) != 2 {
		t.Errorf("PROPFIND returned %d responses, want 2 (the folder and its one file)", len(ms.Responses))
	}
	var sawFile bool
	for _, r := range ms.Responses {
		if strings.HasSuffix(r.Href, "note.txt") {
			sawFile = true
			for _, ps := range r.Propstat {
				if ps.Prop.ContentLength != "" && ps.Prop.ContentLength != "26" {
					t.Errorf("getcontentlength = %q, want 26", ps.Prop.ContentLength)
				}
			}
		}
	}
	if !sawFile {
		t.Error("PROPFIND listing did not include the file")
	}

	// MOVE — rename and relocate in one operation, which is what MOVE means.
	rec = f.dav("MOVE", "/dav/Documents/note.txt", pw, "", map[string]string{
		"Destination": "/dav/renamed.txt",
		"Overwrite":   "F",
	})
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("MOVE = %d: %s", rec.Code, rec.Body)
	}
	if rec := f.dav("GET", "/dav/renamed.txt", pw, "", nil); rec.Code != http.StatusOK {
		t.Errorf("GET after MOVE = %d", rec.Code)
	}
	if rec := f.dav("GET", "/dav/Documents/note.txt", pw, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET of the old path after MOVE = %d, want 404", rec.Code)
	}

	// DELETE — into the trash, not gone. A client deleting the wrong folder
	// should be a recoverable accident.
	if rec := f.dav("DELETE", "/dav/renamed.txt", pw, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", rec.Code, rec.Body)
	}
	if rec := f.dav("GET", "/dav/renamed.txt", pw, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", rec.Code)
	}

	trash := f.do(http.MethodGet, "/api/v1/trash", nil, f.cookie)
	if items := decode(t, trash)["items"].([]any); len(items) != 1 {
		t.Errorf("trash holds %d items after a WebDAV DELETE, want 1", len(items))
		// If this fires with the node row GONE (not merely untrashed), some
		// test in another package is purging trash globally against the shared
		// database — that happened once: files' TestAutoPurgeRespectsRetention
		// called AutoPurgeTrash with zero retention and deleted this fixture's
		// row mid-test. The dump distinguishes the two failure shapes.
		rows, qerr := f.pool.Query(f.ctx, `
			SELECT id, kind, name, path, trashed_at, trashed_root_id
			FROM nodes WHERE owner_id = $1 ORDER BY path`, f.userID)
		if qerr != nil {
			t.Logf("diag query: %v", qerr)
		} else {
			defer rows.Close()
			for rows.Next() {
				var id, kind, name, path string
				var ta, tr any
				_ = rows.Scan(&id, &kind, &name, &path, &ta, &tr)
				t.Logf("node id=%s kind=%s name=%q path=%q trashed_at=%v trashed_root=%v",
					id, kind, name, path, ta, tr)
			}
		}
	}
}

func TestDavPutOverwritesInPlace(t *testing.T) {
	// Saving a file from an editor is a PUT over an existing path. It must
	// produce a new version of the same node, not a duplicate.
	f := newAPIFixture(t)
	pw := f.appPassword("editor")

	f.dav("PUT", "/dav/doc.txt", pw, "first draft", nil)
	f.dav("PUT", "/dav/doc.txt", pw, "second draft, longer", nil)

	rec := f.dav("GET", "/dav/doc.txt", pw, "", nil)
	if rec.Body.String() != "second draft, longer" {
		t.Errorf("content = %q, want the second draft", rec.Body.String())
	}

	rec = f.dav("PROPFIND", "/dav/", pw, "", map[string]string{"Depth": "1"})
	var ms multistatus
	if err := xml.Unmarshal(rec.Body.Bytes(), &ms); err != nil {
		t.Fatal(err)
	}
	if len(ms.Responses) != 2 {
		t.Errorf("root holds %d entries, want 2 (itself and one file) — the PUT duplicated the node", len(ms.Responses))
	}
}

func TestDavEmptyFile(t *testing.T) {
	// `touch` over a mount. A zero-byte PUT is legitimate and must not error.
	f := newAPIFixture(t)
	pw := f.appPassword("shell")

	rec := f.dav("PUT", "/dav/empty.txt", pw, "", nil)
	if rec.Code >= 400 {
		t.Fatalf("PUT of an empty file = %d: %s", rec.Code, rec.Body)
	}
	rec = f.dav("GET", "/dav/empty.txt", pw, "", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("GET of an empty file = %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestDavRejectsInvalidNames(t *testing.T) {
	// The naming rules live in files.ValidateName. WebDAV must not be a way
	// around them, or a mount could create nodes the web UI cannot address.
	f := newAPIFixture(t)
	pw := f.appPassword("client")

	for _, path := range []string{"/dav/CON", "/dav/trailing.", "/dav/has%3Acolon"} {
		rec := f.dav("PUT", path, pw, "x", nil)
		if rec.Code < 400 {
			t.Errorf("PUT %s = %d, want a rejection", path, rec.Code)
		}
	}
}

func TestDavIsOwnerScoped(t *testing.T) {
	// The FileSystem closes over the owner, so there is no path that could
	// address another user's tree — but that is worth proving rather than
	// asserting.
	f := newAPIFixture(t)
	other := newAPIFixture(t)

	fPassword := f.appPassword("mine")
	otherPassword := other.appPassword("theirs")

	f.dav("PUT", "/dav/secret.txt", fPassword, "confidential", nil)

	if rec := other.dav("GET", "/dav/secret.txt", otherPassword, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("another user's GET = %d, want 404", rec.Code)
	}

	// And their root listing shows nothing of ours.
	rec := other.dav("PROPFIND", "/dav/", otherPassword, "", map[string]string{"Depth": "1"})
	if strings.Contains(rec.Body.String(), "secret.txt") {
		t.Error("another user's PROPFIND listed our file")
	}
}

func TestDavRevokedAppPasswordStopsWorking(t *testing.T) {
	f := newAPIFixture(t)
	pw := f.appPassword("temporary")

	if rec := f.dav("PROPFIND", "/dav/", pw, "", map[string]string{"Depth": "0"}); rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND before revocation = %d", rec.Code)
	}

	list := f.do(http.MethodGet, "/api/v1/auth/app-passwords", nil, f.cookie)
	items := decode(t, list)["app_passwords"].([]any)
	id := items[0].(map[string]any)["id"].(string)

	if rec := f.do(http.MethodDelete, "/api/v1/auth/app-passwords/"+id, nil, f.cookie); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}

	if rec := f.dav("PROPFIND", "/dav/", pw, "", map[string]string{"Depth": "0"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("PROPFIND after revocation = %d, want 401", rec.Code)
	}
}

func TestAppPasswordCannotReachTheJSONAPI(t *testing.T) {
	// The containment story: a leaked app password reaches files and nothing
	// else. It must not be able to enrol a passkey, read recovery codes, or
	// mint another credential.
	f := newAPIFixture(t)
	pw := f.appPassword("scoped")

	for _, path := range []string{
		"/api/v1/auth/me",
		"/api/v1/nodes/root",
		"/api/v1/auth/app-passwords",
		"/api/v1/usage",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth(f.username, pw)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("app password reached %s: %d, want 401", path, rec.Code)
		}
	}
}

func TestDavLockingIsAdvertised(t *testing.T) {
	// Finder and Office both LOCK before writing; a server that answers 405
	// makes them refuse to save.
	f := newAPIFixture(t)
	pw := f.appPassword("locker")
	f.dav("PUT", "/dav/locked.txt", pw, "content", nil)

	rec := f.dav("OPTIONS", "/dav/locked.txt", pw, "", nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS = %d", rec.Code)
	}
	if dav := rec.Header().Get("DAV"); !strings.Contains(dav, "1") {
		t.Errorf("DAV header = %q, want at least class 1", dav)
	}

	body := `<?xml version="1.0" encoding="utf-8"?>
<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope>
<D:locktype><D:write/></D:locktype></D:lockinfo>`
	rec = f.dav("LOCK", "/dav/locked.txt", pw, body, map[string]string{
		"Content-Type": "application/xml",
		"Timeout":      "Second-300",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("LOCK = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Lock-Token") == "" {
		t.Error("LOCK returned no Lock-Token")
	}
}
