package httpapi_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// tus 1.0.0 end-to-end. The interesting cases are all failure cases: a happy
// path upload proves very little about a protocol whose entire purpose is
// surviving interruption.

func meta(pairs map[string]string) string {
	var parts []string
	for k, v := range pairs {
		parts = append(parts, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return strings.Join(parts, ",")
}

func (f *apiFixture) tusCreate(parent, name string, length int) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(length))
	req.Header.Set("Upload-Metadata", meta(map[string]string{
		"filename": name, "parent_id": parent,
	}))
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *apiFixture) tusPatch(loc string, offset int, chunk string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPatch, loc, strings.NewReader(chunk))
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Upload-Offset", strconv.Itoa(offset))
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *apiFixture) tusHead(loc string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodHead, loc, nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestTusOptionsAdvertisesCapabilities(t *testing.T) {
	f := newAPIFixture(t)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/uploads", nil)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Tus-Version") != "1.0.0" {
		t.Errorf("Tus-Version = %q", rec.Header().Get("Tus-Version"))
	}
	for _, ext := range []string{"creation", "termination", "expiration"} {
		if !strings.Contains(rec.Header().Get("Tus-Extension"), ext) {
			t.Errorf("Tus-Extension missing %q: %q", ext, rec.Header().Get("Tus-Extension"))
		}
	}
	if rec.Header().Get("Tus-Max-Size") == "" {
		t.Error("Tus-Max-Size missing; a client cannot know what it may send")
	}
}

func TestTusUploadInChunks(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	const content = "chunk-one|chunk-two|chunk-three"
	rec := f.tusCreate(root, "resumable.txt", len(content))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("no Location header; the client has no URL to upload to")
	}
	if rec.Header().Get("Upload-Expires") == "" {
		t.Error("no Upload-Expires; a client cannot tell when resumption stops working")
	}

	// Three chunks, checking the reported offset after each.
	sent := 0
	for _, chunk := range []string{"chunk-one|", "chunk-two|", "chunk-three"} {
		rec = f.tusPatch(loc, sent, chunk)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("patch at %d = %d: %s", sent, rec.Code, rec.Body)
		}
		sent += len(chunk)
		if got := rec.Header().Get("Upload-Offset"); got != strconv.Itoa(sent) {
			t.Fatalf("Upload-Offset = %q, want %d", got, sent)
		}
	}

	// The final chunk completes the upload, so the file exists now — tus has
	// no commit step, and requiring one would strand completed uploads
	// whenever a client disconnected after its last chunk.
	id := rec.Header().Get("X-Node-Id")
	if id == "" {
		t.Fatal("completing PATCH did not report the created node")
	}

	dl := f.do(http.MethodGet, "/api/v1/nodes/"+id+"/content", nil, f.cookie)
	if dl.Code != http.StatusOK {
		t.Fatalf("download = %d", dl.Code)
	}
	if dl.Body.String() != content {
		t.Errorf("reassembled content = %q, want %q", dl.Body.String(), content)
	}

	// The session is gone once the file exists.
	if rec := f.tusHead(loc); rec.Code != http.StatusNotFound {
		t.Errorf("HEAD on a finished upload = %d, want 404", rec.Code)
	}
}

func TestTusHeadReportsResumePoint(t *testing.T) {
	// The whole point of the protocol: after a dropped connection, the client
	// asks where to resume from.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	loc := f.tusCreate(root, "partial.bin", 20).Header().Get("Location")
	f.tusPatch(loc, 0, "0123456789")

	rec := f.tusHead(loc)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", rec.Code)
	}
	if rec.Header().Get("Upload-Offset") != "10" {
		t.Errorf("Upload-Offset = %q, want 10", rec.Header().Get("Upload-Offset"))
	}
	if rec.Header().Get("Upload-Length") != "20" {
		t.Errorf("Upload-Length = %q, want 20", rec.Header().Get("Upload-Length"))
	}
	// A cached HEAD would tell a resuming client to restart from a stale
	// offset — the one answer that must never be wrong.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	// Resume and finish.
	rec = f.tusPatch(loc, 10, "abcdefghij")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("resume = %d: %s", rec.Code, rec.Body)
	}
	id := rec.Header().Get("X-Node-Id")

	dl := f.do(http.MethodGet, "/api/v1/nodes/"+id+"/content", nil, f.cookie)
	if dl.Body.String() != "0123456789abcdefghij" {
		t.Errorf("resumed content = %q", dl.Body.String())
	}
}

func TestTusRejectsWrongOffset(t *testing.T) {
	// Accepting a mismatched offset would either duplicate or skip a range of
	// the file, and the corruption would only surface much later.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	loc := f.tusCreate(root, "offsets.bin", 20).Header().Get("Location")

	f.tusPatch(loc, 0, "0123456789")

	if rec := f.tusPatch(loc, 0, "again"); rec.Code != http.StatusConflict {
		t.Errorf("replayed chunk = %d, want 409", rec.Code)
	}
	if rec := f.tusPatch(loc, 15, "skipahead"); rec.Code != http.StatusConflict {
		t.Errorf("skipped-ahead chunk = %d, want 409", rec.Code)
	}

	// The offset is unchanged after both rejections.
	if got := f.tusHead(loc).Header().Get("Upload-Offset"); got != "10" {
		t.Errorf("offset moved to %q after rejected writes", got)
	}
}

func TestTusRetryAfterPartialChunkIsIdempotent(t *testing.T) {
	// The realistic failure: a chunk is accepted, the response is lost, and the
	// client re-sends from where it thinks it is. HEAD then PATCH must converge
	// on the right file rather than splicing the chunk in twice.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	const content = "AAAABBBBCCCC"
	loc := f.tusCreate(root, "retried.txt", len(content)).Header().Get("Location")

	f.tusPatch(loc, 0, "AAAA")
	f.tusPatch(loc, 4, "BBBB")
	// Client did not see the second response and retries it; the server
	// refuses, the client re-reads the offset and continues correctly.
	if rec := f.tusPatch(loc, 4, "BBBB"); rec.Code != http.StatusConflict {
		t.Fatalf("retry of an accepted chunk = %d, want 409", rec.Code)
	}
	offset, _ := strconv.Atoi(f.tusHead(loc).Header().Get("Upload-Offset"))
	rec := f.tusPatch(loc, offset, content[offset:])
	if rec.Code != http.StatusNoContent {
		t.Fatalf("continue after retry = %d: %s", rec.Code, rec.Body)
	}

	dl := f.do(http.MethodGet, "/api/v1/nodes/"+rec.Header().Get("X-Node-Id")+"/content", nil, f.cookie)
	if dl.Body.String() != content {
		t.Errorf("content after a retried chunk = %q, want %q", dl.Body.String(), content)
	}
}

func TestTusRejectsOverlongUpload(t *testing.T) {
	// Without this a client could ignore its own declared length and fill the
	// disk regardless of the quota checked at creation.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	loc := f.tusCreate(root, "toolong.bin", 5).Header().Get("Location")

	if rec := f.tusPatch(loc, 0, "way more than five bytes"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("overlong chunk = %d, want 413", rec.Code)
	}
}

func TestTusRequiresUploadLength(t *testing.T) {
	// Upload-Defer-Length is deliberately unsupported: without a declared size
	// there is no way to check quota before accepting the bytes.
	f := newAPIFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Defer-Length", "1")
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("create without Upload-Length = %d, want 400", rec.Code)
	}
}

func TestTusRejectsWrongProtocolVersion(t *testing.T) {
	f := newAPIFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "0.2.2")
	req.Header.Set("Upload-Length", "10")
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("old protocol version = %d, want 412", rec.Code)
	}
}

func TestTusRejectsWrongContentType(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	loc := f.tusCreate(root, "typed.bin", 4).Header().Get("Location")

	req := httptest.NewRequest(http.MethodPatch, loc, strings.NewReader("abcd"))
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Upload-Offset", "0")
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("wrong Content-Type = %d, want 415", rec.Code)
	}
}

func TestTusTerminationDiscardsTheUpload(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	loc := f.tusCreate(root, "abandoned.bin", 100).Header().Get("Location")
	f.tusPatch(loc, 0, "partial data")

	if rec := f.do(http.MethodDelete, loc, nil, f.cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("terminate = %d: %s", rec.Code, rec.Body)
	}
	if rec := f.tusHead(loc); rec.Code != http.StatusNotFound {
		t.Errorf("HEAD after termination = %d, want 404", rec.Code)
	}

	// And the bytes are gone, not merely unreferenced.
	admin := f.do(http.MethodPost, "/api/v1/admin/fsck", nil, f.admin)
	if admin.Code != http.StatusOK {
		t.Fatalf("fsck = %d", admin.Code)
	}
	if orphans := decode(t, admin)["orphans"].(float64); orphans != 0 {
		t.Errorf("terminated upload left %v orphan(s) on disk", orphans)
	}
}

func TestTusUploadsAreOwnerScoped(t *testing.T) {
	f := newAPIFixture(t)
	other := newAPIFixture(t)

	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))
	loc := f.tusCreate(root, "private.bin", 10).Header().Get("Location")

	for _, tc := range []struct{ method string }{{http.MethodHead}, {http.MethodDelete}} {
		req := httptest.NewRequest(tc.method, loc, nil)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.AddCookie(other.cookie)
		rec := httptest.NewRecorder()
		other.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s another user's upload = %d, want 404", tc.method, rec.Code)
		}
	}
}

func TestTusRespectsQuotaBeforeAcceptingBytes(t *testing.T) {
	// Discovering at 99% that a file will not fit is the worst possible moment
	// to find out, and it is entirely avoidable when the length is declared.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE users SET quota_bytes = 10 WHERE id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}

	rec := f.tusCreate(root, "toobig.bin", 1000)
	if rec.Code != http.StatusInsufficientStorage {
		t.Errorf("create beyond quota = %d, want 507", rec.Code)
	}
}

func TestTusUploadMetadataDecoding(t *testing.T) {
	// Filenames are not ASCII, which is why the header is base64.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	const name = "日本語のファイル.txt"
	rec := f.tusCreate(root, name, 2)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body)
	}
	rec = f.tusPatch(rec.Header().Get("Location"), 0, "hi")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body)
	}

	got := f.do(http.MethodGet, "/api/v1/nodes/"+rec.Header().Get("X-Node-Id"), nil, f.cookie)
	if n := decode(t, got)["node"].(map[string]any); n["name"] != name {
		t.Errorf("name = %v, want %q", n["name"], name)
	}
}

func TestTusZeroByteUpload(t *testing.T) {
	// A zero-length file is legitimate, and the completion check must fire on
	// creation rather than waiting for a chunk that will never come.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	loc := f.tusCreate(root, "empty.txt", 0).Header().Get("Location")
	rec := f.tusPatch(loc, 0, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("empty patch = %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Node-Id") == "" {
		t.Fatal("zero-byte upload did not complete")
	}
}

func TestTusRequiresAuthentication(t *testing.T) {
	f := newAPIFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "10")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("create without a session = %d, want 401", rec.Code)
	}
}

func TestTusResponsesCarryProtocolHeader(t *testing.T) {
	// A bare error with no Tus-Resumable leaves a client unable to tell "wrong
	// URL" from "this server does not speak tus".
	f := newAPIFixture(t)

	rec := f.tusHead(fmt.Sprintf("/api/v1/uploads/%s", "00000000-0000-0000-0000-000000000000"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD unknown upload = %d, want 404", rec.Code)
	}
	if rec.Header().Get("Tus-Resumable") != "1.0.0" {
		t.Error("error response is missing Tus-Resumable")
	}
}
