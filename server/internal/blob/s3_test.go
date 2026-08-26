package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is an in-process object store that behaves like the subset of S3 this
// package uses — and, crucially, VERIFIES THE SIGNATURE the way a real service
// does. A fake that accepted any Authorization header would let every SigV4
// mistake through, and every one of those mistakes has exactly one symptom in
// production (403 SignatureDoesNotMatch) which is impossible to debug from a
// log line. The verification below is written out longhand rather than calling
// S3Store.sign, so the test is checking the algorithm and not its own echo.
type fakeS3 struct {
	t      *testing.T
	bucket string
	keyID  string
	secret string
	region string

	mu      sync.Mutex
	objects map[string][]byte

	// ignoreRange makes the fake answer 200 with the whole object even when a
	// Range was asked for — the behaviour of a gateway that does not implement
	// ranges, which s3Reader has to survive without serving the wrong bytes.
	ignoreRange bool
	// failNextPut makes one PUT fail, for the demotion-safety test.
	failNextPut bool
}

func newFakeS3(t *testing.T) (*fakeS3, *S3Store) {
	t.Helper()
	f := &fakeS3{
		t: t, bucket: "cold", keyID: "AKIAEXAMPLE", secret: "s3cr3t", region: "eu-west-2",
		objects: map[string][]byte{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	store, err := NewS3Store(S3Config{
		Endpoint:   srv.URL,
		Bucket:     f.bucket,
		Region:     f.region,
		AccessKey:  f.keyID,
		SecretKey:  f.secret,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	return f, store
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := f.verify(r); err != nil {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<Error><Code>SignatureDoesNotMatch</Code><Message>%s</Message></Error>`, err)
		return
	}

	path := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	bucket, obj, _ := strings.Cut(path, "/")
	if bucket != f.bucket {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code><Message>no</Message></Error>`))
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		if obj == "" && r.URL.Query().Get("list-type") == "2" {
			f.list(w, r)
			return
		}
		f.get(w, r, obj)
	case http.MethodHead:
		body, ok := f.objects[obj]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
	case http.MethodPut:
		if f.failNextPut {
			f.failNextPut = false
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`<Error><Code>InternalError</Code><Message>nope</Message></Error>`))
			return
		}
		if src := r.Header.Get("x-amz-copy-source"); src != "" {
			from := strings.TrimPrefix(src, "/"+f.bucket+"/")
			body, ok := f.objects[from]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			f.objects[obj] = append([]byte(nil), body...)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<CopyObjectResult></CopyObjectResult>`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.objects[obj] = body
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(f.objects, obj)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) get(w http.ResponseWriter, r *http.Request, obj string) {
	body, ok := f.objects[obj]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>gone</Message></Error>`))
		return
	}
	rng := r.Header.Get("Range")
	if rng == "" || f.ignoreRange {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	var start int64
	if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil || start > int64(len(body)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)-int(start)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body[start:])
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(`<ListBucketResult><IsTruncated>false</IsTruncated>`)
	for _, k := range keys {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size></Contents>`,
			xmlEscape(k), len(f.objects[k]))
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, b.String())
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// verify recomputes the SigV4 signature from the request as received.
func (f *fakeS3) verify(r *http.Request) error {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return errors.New("missing or malformed Authorization header")
	}
	var credential, signedHeaders, signature string
	for _, part := range strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "Credential":
			credential = v
		case "SignedHeaders":
			signedHeaders = v
		case "Signature":
			signature = v
		}
	}
	amzDate := r.Header.Get("x-amz-date")
	if amzDate == "" {
		return errors.New("no x-amz-date")
	}
	dateStamp := amzDate[:8]
	scope := dateStamp + "/" + f.region + "/s3/aws4_request"
	if credential != f.keyID+"/"+scope {
		return fmt.Errorf("credential scope %q, want %q", credential, f.keyID+"/"+scope)
	}
	if !strings.Contains(signedHeaders, "host") ||
		!strings.Contains(signedHeaders, "x-amz-content-sha256") {
		return fmt.Errorf("signed headers %q must cover host and the payload hash", signedHeaders)
	}

	var canon strings.Builder
	for _, h := range strings.Split(signedHeaders, ";") {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		fmt.Fprintf(&canon, "%s:%s\n", h, strings.TrimSpace(v))
	}
	uri := r.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	canonicalRequest := strings.Join([]string{
		r.Method, uri, canonicalQuery(r.URL.Query()), canon.String(), signedHeaders,
		r.Header.Get("x-amz-content-sha256"),
	}, "\n")
	crHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(crHash[:]),
	}, "\n")

	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	k := mac([]byte("AWS4"+f.secret), dateStamp)
	k = mac(k, f.region)
	k = mac(k, "s3")
	k = mac(k, "aws4_request")
	if want := hex.EncodeToString(mac(k, stringToSign)); want != signature {
		return fmt.Errorf("signature mismatch for %s %s", r.Method, uri)
	}
	return nil
}

// --- the Store contract -----------------------------------------------------

func TestS3PutOpenRoundTrip(t *testing.T) {
	_, s := newFakeS3(t)
	ctx := context.Background()
	payload := []byte("the quick brown fox jumps over the lazy dog")

	res, err := s.Put(ctx, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Size != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", res.Size, len(payload))
	}
	want := sha256.Sum256(payload)
	if !bytes.Equal(res.SHA256, want[:]) {
		t.Error("Put returned the wrong content hash")
	}
	if parts := strings.Split(res.Key, "/"); len(parts) != 3 {
		t.Errorf("key %q should share the ab/cd/abcd... layout with the hot tier", res.Key)
	}

	rc, err := s.Open(ctx, res.Key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("content read back does not match what was written")
	}
}

func TestS3OpenIsSeekable(t *testing.T) {
	// The reason the Store contract insists on a ReadSeekCloser matters more
	// over a network than on a disk: discarding a prefix means paying egress for
	// bytes nobody asked for.
	_, s := newFakeS3(t)
	ctx := context.Background()

	res, err := s.Put(ctx, strings.NewReader("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := s.Open(ctx, res.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	if _, err := rc.Seek(4, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != "456789" {
		t.Errorf("after seek got %q, want %q", got, "456789")
	}

	// Backwards, then the ServeContent probe: seek to the end for a length and
	// straight back to the start.
	if _, err := rc.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if n, err := rc.Seek(0, io.SeekStart); err != nil || n != 0 {
		t.Fatalf("Seek(0, start) = %d, %v", n, err)
	}
	got, _ = io.ReadAll(rc)
	if string(got) != "0123456789" {
		t.Errorf("after rewinding got %q", got)
	}
}

func TestS3SeekSurvivesAStoreThatIgnoresRange(t *testing.T) {
	// A gateway that answers 200 with the whole object for a ranged GET must not
	// make the reader serve the wrong bytes from the wrong offset. Correct and
	// slow beats fast and corrupt.
	f, s := newFakeS3(t)
	f.ignoreRange = true
	ctx := context.Background()

	res, err := s.Put(ctx, strings.NewReader("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := s.Open(ctx, res.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	if _, err := rc.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != "789" {
		t.Errorf("got %q, want %q — the ignored Range was not compensated for", got, "789")
	}
}

func TestS3PutKeyedIsIdempotent(t *testing.T) {
	// Dedup means the same chunk arrives repeatedly and the key IS the content
	// hash, so an existing object already holds what would be written. On a cold
	// tier the saved upload is also the saved bill.
	f, s := newFakeS3(t)
	ctx := context.Background()

	existed, err := s.PutKeyed(ctx, "ab/cd/abcdef", strings.NewReader("chunk"))
	if err != nil {
		t.Fatalf("PutKeyed: %v", err)
	}
	if existed {
		t.Error("first PutKeyed reported the key as already present")
	}

	// A second write of the same key must not touch the object at all — proved
	// by making the store refuse any PUT that reaches it.
	f.mu.Lock()
	f.failNextPut = true
	f.mu.Unlock()

	existed, err = s.PutKeyed(ctx, "ab/cd/abcdef", strings.NewReader("chunk"))
	if err != nil {
		t.Fatalf("second PutKeyed: %v", err)
	}
	if !existed {
		t.Error("second PutKeyed should report the key as already present")
	}

	f.mu.Lock()
	f.failNextPut = false
	f.mu.Unlock()

	rc, err := s.Open(ctx, "ab/cd/abcdef")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "chunk" {
		t.Errorf("stored %q, want %q", got, "chunk")
	}
}

func TestS3StatAndDelete(t *testing.T) {
	_, s := newFakeS3(t)
	ctx := context.Background()

	if _, err := s.Stat(ctx, "no/su/nosuch"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat of a missing key = %v, want ErrNotFound", err)
	}
	if _, err := s.Open(ctx, "no/su/nosuch"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open of a missing key = %v, want ErrNotFound", err)
	}

	res, err := s.Put(ctx, strings.NewReader("bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.Stat(ctx, res.Key); err != nil || n != 5 {
		t.Errorf("Stat = %d, %v; want 5, nil", n, err)
	}

	// Idempotent, for the same reason the filesystem store's is: a GC retry must
	// not turn into a permanent failure.
	if err := s.Delete(ctx, res.Key); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := s.Delete(ctx, res.Key); err != nil {
		t.Fatalf("second Delete should be a no-op, got %v", err)
	}
}

func TestS3PutEmpty(t *testing.T) {
	// A zero-byte file is legitimate content and must round-trip through the
	// cold tier exactly as it does through the hot one.
	_, s := newFakeS3(t)
	ctx := context.Background()

	res, err := s.Put(ctx, bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("Put(empty): %v", err)
	}
	if res.Size != 0 {
		t.Errorf("Size = %d, want 0", res.Size)
	}
	if n, err := s.Stat(ctx, res.Key); err != nil || n != 0 {
		t.Errorf("Stat = %d, %v; want 0, nil", n, err)
	}
	rc, err := s.Open(ctx, res.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if got, _ := io.ReadAll(rc); len(got) != 0 {
		t.Errorf("read %d bytes from an empty object", len(got))
	}
}

func TestS3RejectsInvalidKeys(t *testing.T) {
	// ONE definition of what a key is, shared with the filesystem store. A key
	// legal in one tier and illegal in the other would strand demoted content.
	_, s := newFakeS3(t)
	ctx := context.Background()

	for _, key := range []string{"", "../etc/passwd", "ab/../../etc/passwd", "/etc/passwd", `ab\cd`, "C:/windows"} {
		if _, err := s.Open(ctx, key); err == nil {
			t.Errorf("Open(%q) succeeded, want rejection", key)
		}
		if err := s.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) succeeded, want rejection", key)
		}
	}
}

func TestS3WalkPagesAndStripsThePrefix(t *testing.T) {
	f, _ := newFakeS3(t)
	store, err := NewS3Store(S3Config{
		Endpoint: "http://example.invalid", Bucket: f.bucket, Region: f.region,
		AccessKey: f.keyID, SecretKey: f.secret, Prefix: "pc",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Point it at the live fake, keeping the prefix.
	srv := httptest.NewServer(f)
	defer srv.Close()
	store.endpoint = srv.URL
	store.client = srv.Client()

	ctx := context.Background()
	if _, err := store.PutKeyed(ctx, "ab/cd/one", strings.NewReader("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutKeyed(ctx, "ef/gh/two", strings.NewReader("22")); err != nil {
		t.Fatal(err)
	}
	// Somebody else's content in the same bucket. Not ours to report on.
	f.mu.Lock()
	f.objects["someone-else/thing"] = []byte("x")
	f.mu.Unlock()

	seen := map[string]int64{}
	if err := store.Walk(ctx, func(key string, size int64) error {
		seen[key] = size
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(seen) != 2 || seen["ab/cd/one"] != 1 || seen["ef/gh/two"] != 2 {
		t.Errorf("Walk returned %v; want the two prefixed keys with the prefix stripped", seen)
	}
}

// --- the Stager contract ----------------------------------------------------

func TestS3StagerRoundTripAndTruncation(t *testing.T) {
	// The same property the filesystem stager is tested for: the caller's
	// committed offset is the single authority, so a resumed upload cannot
	// splice a duplicated span into the middle of a file.
	_, s := newFakeS3(t)
	ctx := context.Background()

	key, err := s.CreatePartial()
	if err != nil {
		t.Fatalf("CreatePartial: %v", err)
	}
	if n, err := s.StatPartial(key); err != nil || n != 0 {
		t.Fatalf("StatPartial of a fresh staging object = %d, %v", n, err)
	}

	h := sha256.New()
	if _, err := s.AppendPartial(ctx, key, 0, h, strings.NewReader("AAAABBBB")); err != nil {
		t.Fatalf("AppendPartial: %v", err)
	}
	if n, _ := s.StatPartial(key); n != 8 {
		t.Fatalf("staging size = %d, want 8", n)
	}

	h2 := sha256.New()
	h2.Write([]byte("AAAA"))
	if _, err := s.AppendPartial(ctx, key, 4, h2, strings.NewReader("CCCC")); err != nil {
		t.Fatalf("resume: %v", err)
	}

	blobKey, err := s.CommitPartial(key)
	if err != nil {
		t.Fatalf("CommitPartial: %v", err)
	}
	if parts := strings.Split(blobKey, "/"); len(parts) != 3 {
		t.Errorf("committed key %q is not in the ab/cd/abcd... layout", blobKey)
	}

	rc, err := s.Open(ctx, blobKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "AAAACCCC" {
		t.Errorf("resumed object = %q, want %q — the uncommitted tail survived", got, "AAAACCCC")
	}
	want := sha256.Sum256([]byte("AAAACCCC"))
	if !bytes.Equal(h2.Sum(nil), want[:]) {
		t.Error("the resumed hash does not describe the resumed content")
	}
	if _, err := s.StatPartial(key); !errors.Is(err, ErrNotFound) {
		t.Error("the staging object survived the commit")
	}
}

func TestS3WalkStagingSeesOnlyPartials(t *testing.T) {
	_, s := newFakeS3(t)
	ctx := context.Background()

	if _, err := s.Put(ctx, strings.NewReader("a real blob")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePartial(); err != nil {
		t.Fatal(err)
	}

	var staged int
	if err := s.WalkStaging(func(string, int64) error { staged++; return nil }); err != nil {
		t.Fatal(err)
	}
	if staged != 1 {
		t.Errorf("WalkStaging found %d, want 1", staged)
	}
}

func TestS3ConfigRefusesIncompleteSettings(t *testing.T) {
	// Loud at construction, not on the first demotion hours later.
	cases := []S3Config{
		{Bucket: "b", AccessKey: "k", SecretKey: "s"},
		{Endpoint: "ftp://nope", Bucket: "b", AccessKey: "k", SecretKey: "s"},
		{Endpoint: "https://s3.example", AccessKey: "k", SecretKey: "s"},
		{Endpoint: "https://s3.example", Bucket: "b", SecretKey: "s"},
		{Endpoint: "https://s3.example", Bucket: "b", AccessKey: "k"},
	}
	for i, c := range cases {
		if _, err := NewS3Store(c); err == nil {
			t.Errorf("case %d: NewS3Store accepted an incomplete configuration", i)
		}
	}
}

func TestS3PutIsCancellable(t *testing.T) {
	_, s := newFakeS3(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Put(ctx, strings.NewReader("never uploaded")); err == nil {
		t.Fatal("Put with a cancelled context succeeded, want error")
	}
}

func TestS3SigningUsesTheRequestTime(t *testing.T) {
	// A pinned clock, so a signature that silently ignored the date would show
	// up here rather than at midnight UTC in production.
	f, s := newFakeS3(t)
	fixed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	s.now = func() time.Time { return fixed }

	if _, err := s.PutKeyed(context.Background(), "aa/bb/aabb", strings.NewReader("x")); err != nil {
		t.Fatalf("PutKeyed under a pinned clock: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects["aa/bb/aabb"]; !ok {
		t.Error("the signed request did not reach the store")
	}
}
