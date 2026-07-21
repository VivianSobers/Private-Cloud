package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/httpapi"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/metrics"
)

// End-to-end tests over the real router, middleware, auth and store.
//
// The session is minted directly through auth.Service rather than by driving a
// WebAuthn ceremony: passkeys need an authenticator, and what is under test
// here is the file API behind requireAuth, not the ceremony itself (which the
// auth package covers).
//
//	PC_TEST_DATABASE_URL=postgres://... go test ./internal/httpapi/...

type apiFixture struct {
	t       *testing.T
	handler http.Handler
	cookie  *http.Cookie
	admin   *http.Cookie
	userID  uuid.UUID
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()

	dsn := os.Getenv("PC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PC_TEST_DATABASE_URL not set; skipping API integration tests")
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	database, err := db.Open(ctx, dsn, 8, 1, 10*time.Second, log)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(database.Close)
	if err := database.Migrate(ctx, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	authStore := auth.NewStore(database.Pool)
	authSvc, err := auth.NewService(authStore, auth.Config{
		RPDisplayName: "Test", RPID: "localhost",
		RPOrigins: []string{"http://localhost"}, SessionTTL: time.Hour,
	}, log)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}

	blobs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	filesSvc := files.NewService(files.NewStore(database.Pool), blobs, log)

	m := metrics.New("test", "test", func() float64 { return 0 })
	srv := httpapi.NewServer(log, database, m, authSvc, filesSvc, httpapi.Options{
		Version: "test", Commit: "test", CookieName: "pc_session",
	})

	f := &apiFixture{t: t, handler: srv.Handler()}
	f.userID, f.cookie = newSession(t, ctx, authStore, authSvc, false)
	_, f.admin = newSession(t, ctx, authStore, authSvc, true)
	return f
}

func newSession(t *testing.T, ctx context.Context, store *auth.Store, svc *auth.Service, admin bool) (uuid.UUID, *http.Cookie) {
	t.Helper()
	name := "api-" + uuid.NewString()[:8]
	user, err := store.CreateUser(ctx, uuid.New(), name, name, admin)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, _, err := svc.NewSessionToken(ctx, user, auth.SessionKindWeb, "integration-test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return user.ID, &http.Cookie{Name: "pc_session", Value: token}
}

func (f *apiFixture) do(method, path string, body io.Reader, cookie *http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, body)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *apiFixture) json(method, path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		r = bytes.NewReader(buf)
	}
	return f.do(method, path, r, f.cookie)
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

func nodeID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := decode(t, rec)
	node, ok := body["node"].(map[string]any)
	if !ok {
		t.Fatalf("no node in response: %s", rec.Body.String())
	}
	return node["id"].(string)
}

// upload posts raw bytes, the primary path: no buffering, no framing overhead.
func (f *apiFixture) upload(parent, name, content string) *httptest.ResponseRecorder {
	f.t.Helper()
	path := fmt.Sprintf("/api/v1/upload?parent_id=%s&name=%s", parent, name)
	return f.do(http.MethodPost, path, strings.NewReader(content), f.cookie)
}

// --- tests ------------------------------------------------------------------

func TestFileAPIRequiresAuthentication(t *testing.T) {
	f := newAPIFixture(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/nodes/root"},
		{http.MethodGet, "/api/v1/usage"},
		{http.MethodGet, "/api/v1/trash"},
		{http.MethodPost, "/api/v1/folders"},
		{http.MethodPost, "/api/v1/upload?name=x"},
	} {
		rec := f.do(tc.method, tc.path, strings.NewReader("{}"), nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestFullFileLifecycleOverHTTP(t *testing.T) {
	f := newAPIFixture(t)

	// root
	rec := f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("get root = %d: %s", rec.Code, rec.Body)
	}
	root := nodeID(t, rec)

	// mkdir
	rec = f.json(http.MethodPost, "/api/v1/folders", map[string]any{
		"parent_id": root, "name": "Documents",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create folder = %d: %s", rec.Code, rec.Body)
	}
	docs := nodeID(t, rec)

	// upload
	const content = "hello from the integration test"
	rec = f.upload(docs, "greeting.txt", content)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body)
	}
	file := decode(t, rec)["node"].(map[string]any)
	if int(file["size"].(float64)) != len(content) {
		t.Errorf("size = %v, want %d", file["size"], len(content))
	}
	if file["path"] != "/Documents/greeting.txt" {
		t.Errorf("path = %v", file["path"])
	}
	fileID := file["id"].(string)

	// listing
	rec = f.do(http.MethodGet, "/api/v1/nodes/"+docs+"/children", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body)
	}
	if children := decode(t, rec)["children"].([]any); len(children) != 1 {
		t.Errorf("listed %d children, want 1", len(children))
	}

	// resolve by path — what WebDAV and the sync client need
	rec = f.do(http.MethodGet, "/api/v1/nodes/resolve?path=/Documents/greeting.txt", nil, f.cookie)
	if rec.Code != http.StatusOK || nodeID(t, rec) != fileID {
		t.Errorf("resolve by path = %d: %s", rec.Code, rec.Body)
	}

	// download
	rec = f.do(http.MethodGet, "/api/v1/nodes/"+fileID+"/content", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d", rec.Code)
	}
	if rec.Body.String() != content {
		t.Errorf("downloaded %q, want %q", rec.Body.String(), content)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("no ETag; clients cannot revalidate cheaply")
	}
	// Without these, a stored HTML or SVG file executes in the app's own
	// origin — same-origin stored XSS with access to the session cookie.
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("missing Content-Security-Policy on downloaded content")
	}

	// rename + move in one call
	rec = f.json(http.MethodPatch, "/api/v1/nodes/"+fileID, map[string]any{
		"name": "renamed.txt", "parent_id": root,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body)
	}
	if p := decode(t, rec)["node"].(map[string]any)["path"]; p != "/renamed.txt" {
		t.Errorf("path after move+rename = %v, want /renamed.txt", p)
	}

	// trash, list, restore
	if rec := f.do(http.MethodDelete, "/api/v1/nodes/"+fileID, nil, f.cookie); rec.Code != http.StatusOK {
		t.Fatalf("trash = %d: %s", rec.Code, rec.Body)
	}
	rec = f.do(http.MethodGet, "/api/v1/trash", nil, f.cookie)
	if items := decode(t, rec)["items"].([]any); len(items) != 1 {
		t.Errorf("trash holds %d items, want 1", len(items))
	}
	if rec := f.do(http.MethodPost, "/api/v1/trash/"+fileID+"/restore", nil, f.cookie); rec.Code != http.StatusOK {
		t.Fatalf("restore = %d: %s", rec.Code, rec.Body)
	}

	// trash again, then purge permanently
	f.do(http.MethodDelete, "/api/v1/nodes/"+fileID, nil, f.cookie)
	if rec := f.do(http.MethodDelete, "/api/v1/trash/"+fileID, nil, f.cookie); rec.Code != http.StatusOK {
		t.Fatalf("purge = %d: %s", rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodGet, "/api/v1/nodes/"+fileID, nil, f.cookie); rec.Code != http.StatusNotFound {
		t.Errorf("purged node still readable: %d", rec.Code)
	}
}

func TestRangeRequestsAreServed(t *testing.T) {
	// Video scrubbing depends on this, and a hand-rolled Range implementation
	// is wrong in ways only scrubbing reveals — hence http.ServeContent.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	rec := f.upload(root, "data.bin", "0123456789")
	id := nodeID(t, rec)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+id+"/content", nil)
	req.AddCookie(f.cookie)
	req.Header.Set("Range", "bytes=2-5")
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if rec.Body.String() != "2345" {
		t.Errorf("range body = %q, want %q", rec.Body.String(), "2345")
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 2-5/10" {
		t.Errorf("Content-Range = %q", cr)
	}
}

func TestConditionalGetReturns304(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	id := nodeID(t, f.upload(root, "cached.txt", "content"))

	rec := f.do(http.MethodGet, "/api/v1/nodes/"+id+"/content", nil, f.cookie)
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag to revalidate with")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+id+"/content", nil)
	req.AddCookie(f.cookie)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Errorf("revalidation = %d, want 304", rec.Code)
	}
}

func TestHeadReturnsSizeWithoutBody(t *testing.T) {
	// ServeMux does not imply HEAD from GET; without an explicit route a client
	// checking a file's size before downloading gets a 405.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	id := nodeID(t, f.upload(root, "sized.txt", "0123456789"))

	rec := f.do(http.MethodHead, "/api/v1/nodes/"+id+"/content", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Length") != "10" {
		t.Errorf("Content-Length = %q, want 10", rec.Header().Get("Content-Length"))
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned a %d-byte body", rec.Body.Len())
	}
}

func TestMultipartUpload(t *testing.T) {
	// A plain HTML <form> cannot produce anything else, and slice 5's UI should
	// not need JavaScript to upload a file.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("description", "an ordinary form field to be skipped")
	part, err := mw.CreateFormFile("file", `C:\Users\someone\report.pdf`)
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("%PDF-1.4 fake"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?parent_id="+root, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("multipart upload = %d: %s", rec.Code, rec.Body)
	}
	node := decode(t, rec)["node"].(map[string]any)
	// A Windows browser really does send a full client-side path.
	if node["name"] != "report.pdf" {
		t.Errorf("name = %v, want report.pdf (the client path must be stripped)", node["name"])
	}
	if node["mime"] != "application/pdf" {
		t.Errorf("mime = %v, want application/pdf", node["mime"])
	}
}

func TestInvalidNamesAreRejected(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	for _, name := range []string{"..", "a/b", "CON", "trailing ", ""} {
		rec := f.json(http.MethodPost, "/api/v1/folders", map[string]any{
			"parent_id": root, "name": name,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("create folder %q = %d, want 400", name, rec.Code)
		}
	}
}

func TestDuplicateNameReturns409(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	body := map[string]any{"parent_id": root, "name": "Photos"}
	if rec := f.json(http.MethodPost, "/api/v1/folders", body); rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d", rec.Code)
	}
	// Folded uniqueness: a macOS or Windows client will try exactly this.
	body["name"] = "photos"
	if rec := f.json(http.MethodPost, "/api/v1/folders", body); rec.Code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", rec.Code)
	}
}

func TestOneUserCannotReachAnothersFiles(t *testing.T) {
	f := newAPIFixture(t)
	other := newAPIFixture(t)

	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	secret := nodeID(t, f.upload(root, "secret.txt", "confidential"))

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/nodes/" + secret},
		{http.MethodGet, "/api/v1/nodes/" + secret + "/content"},
		{http.MethodDelete, "/api/v1/nodes/" + secret},
	} {
		rec := other.do(tc.method, tc.path, nil, other.cookie)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as another user = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

func TestUsageReflectsUploads(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	f.upload(root, "a.txt", "12345")

	rec := f.do(http.MethodGet, "/api/v1/usage", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage = %d", rec.Code)
	}
	body := decode(t, rec)
	if body["live_bytes"].(float64) != 5 {
		t.Errorf("live_bytes = %v, want 5", body["live_bytes"])
	}
	if body["file_count"].(float64) != 1 {
		t.Errorf("file_count = %v, want 1", body["file_count"])
	}
}

func TestFsckRequiresAdmin(t *testing.T) {
	// fsck walks the entire blob store and can delete orphans.
	f := newAPIFixture(t)

	if rec := f.do(http.MethodPost, "/api/v1/admin/fsck", nil, f.cookie); rec.Code != http.StatusForbidden {
		t.Errorf("fsck as a normal user = %d, want 403", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/api/v1/admin/fsck", nil, f.admin); rec.Code != http.StatusOK {
		t.Errorf("fsck as admin = %d, want 200", rec.Code)
	}
}

func TestUploadRequiresAName(t *testing.T) {
	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, "/api/v1/upload", strings.NewReader("body"), f.cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("nameless raw upload = %d, want 400", rec.Code)
	}
}
