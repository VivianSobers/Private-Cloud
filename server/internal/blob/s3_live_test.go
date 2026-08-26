package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Live tests against a REAL S3 implementation.
//
// Everything in s3_test.go runs against an in-process fake, which proves this
// package sends what it thinks it sends. It cannot prove the other half: that
// a real server AGREES. SigV4 is a signature over a canonical request, and
// every disagreement about canonicalisation — a header this code signs and the
// server does not, an escaped slash, a trailing newline — produces the same
// opaque 403, from a fake that was written by the same person who wrote the
// signer and therefore shares its assumptions.
//
// That gap is worth closing here rather than in production, because the caller
// least able to survive it is fsck: content "moved to cold" by code that cannot
// read it back is content that is gone.
//
// Point PC_TEST_S3_* at MinIO, Garage or real S3 and these run; unset, they
// skip, so the ordinary `go test ./...` on a machine with no bucket is
// unaffected.
const (
	envS3Endpoint = "PC_TEST_S3_ENDPOINT"
	envS3Bucket   = "PC_TEST_S3_BUCKET"
	envS3Key      = "PC_TEST_S3_ACCESS_KEY"
	envS3Secret   = "PC_TEST_S3_SECRET_KEY"
)

// liveStore builds a store against the configured server, or skips.
//
// Each test gets its own key prefix so a failed run leaves no debris that the
// next one has to reason about, and so two runs cannot collide in one bucket.
func liveStore(t *testing.T) *S3Store {
	t.Helper()

	endpoint := os.Getenv(envS3Endpoint)
	if endpoint == "" {
		t.Skipf("%s is not set; skipping the live S3 tests", envS3Endpoint)
	}
	bucket := os.Getenv(envS3Bucket)
	if bucket == "" {
		t.Fatalf("%s is set but %s is not", envS3Endpoint, envS3Bucket)
	}

	prefix := "livetest/" + strings.ReplaceAll(t.Name(), "/", "_") + "/"
	store, err := NewS3Store(S3Config{
		Endpoint:   endpoint,
		Bucket:     bucket,
		Region:     envOr("PC_TEST_S3_REGION", "us-east-1"),
		AccessKey:  os.Getenv(envS3Key),
		SecretKey:  os.Getenv(envS3Secret),
		Prefix:     prefix,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	})
	if err != nil {
		t.Fatalf("building the live store: %v", err)
	}

	t.Cleanup(func() {
		// Best effort: a leftover object costs a few bytes, and failing a test
		// during cleanup would hide whatever the test actually found.
		_ = store.Walk(context.Background(), func(key string, _ int64) error {
			_ = store.Delete(context.Background(), key)
			return nil
		})
	})
	return store
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// The round trip that decides whether the cold tier is safe at all. If a real
// server accepts a PUT and then hands back different bytes — or refuses the
// signature entirely — every other property of the tier is irrelevant.
func TestLiveS3RoundTripsExactBytes(t *testing.T) {
	store := liveStore(t)
	ctx := context.Background()

	// Deliberately not a round number and not text: a size that is a multiple
	// of a buffer, or a body that survives a charset mangling, is a body that
	// can hide a bug in chunking or in content-length.
	payload := make([]byte, 1<<20+7919)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)

	res, err := store.Put(ctx, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put against a real server: %v", err)
	}
	if res.Size != int64(len(payload)) {
		t.Errorf("Put reported %d bytes, wrote %d", res.Size, len(payload))
	}
	if !bytes.Equal(res.SHA256, want[:]) {
		t.Error("Put's hash does not match the bytes it was given")
	}

	rc, err := store.Open(ctx, res.Key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read back %d bytes, wrote %d, and they differ", len(got), len(payload))
	}

	size, err := store.Stat(ctx, res.Key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if size != int64(len(payload)) {
		t.Errorf("Stat = %d, want %d", size, len(payload))
	}
}

// Range requests, against a server that really implements them.
//
// The fake was written to honour Range and to ignore it, so both paths are
// covered there — but only in the shapes this package anticipated. A real
// server's 206, its Content-Range and its behaviour at the end of the object
// are the things worth confirming, because Open promises a ReadSeekCloser and
// the download handler seeks for every video scrub.
func TestLiveS3OpenSeeksWithoutRefetching(t *testing.T) {
	store := liveStore(t)
	ctx := context.Background()

	payload := []byte(strings.Repeat("0123456789abcdef", 4096)) // 64 KiB
	res, err := store.Put(ctx, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	rc, err := store.Open(ctx, res.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	if _, err := rc.Seek(1024, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("ReadFull after seek: %v", err)
	}
	if !bytes.Equal(buf, payload[1024:1040]) {
		t.Errorf("after seeking to 1024 got %q, want %q", buf, payload[1024:1040])
	}

	// Seeking relative to the end is what a client asking for the last bytes of
	// a file does, and it is the case a naive Range implementation gets wrong.
	if _, err := rc.Seek(-16, io.SeekEnd); err != nil {
		t.Fatalf("Seek from end: %v", err)
	}
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("ReadFull at end: %v", err)
	}
	if !bytes.Equal(buf, payload[len(payload)-16:]) {
		t.Errorf("tail = %q, want %q", buf, payload[len(payload)-16:])
	}
}

// PutKeyed's idempotence is what dedup rests on: the key IS the content hash,
// so an existing key already holds exactly what would be written. Confirming
// that a real server reports the existing object rather than rewriting it is
// the difference between dedup and paying for every duplicate twice.
func TestLiveS3PutKeyedIsIdempotent(t *testing.T) {
	store := liveStore(t)
	ctx := context.Background()

	const key = "abc123def456"
	body := []byte("content-addressed bytes")

	existed, err := store.PutKeyed(ctx, key, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("first PutKeyed: %v", err)
	}
	if existed {
		t.Error("the first write reported the key as already present")
	}

	existed, err = store.PutKeyed(ctx, key, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("second PutKeyed: %v", err)
	}
	if !existed {
		t.Error("the second write did not report the key as already present")
	}

	rc, err := store.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Errorf("read back %q, want %q", got, body)
	}
}

// The absence that fsck reads. A missing object must come back as ErrNotFound
// and never as a generic error, because classifyAbsent branches on exactly that
// distinction: ErrNotFound on a cold row is reported as lost, and anything else
// is reported as unverified. Getting this wrong against a real server is how
// fsck would send somebody to a backup for content that is fine.
func TestLiveS3MissingObjectIsErrNotFound(t *testing.T) {
	store := liveStore(t)
	ctx := context.Background()

	if _, err := store.Stat(ctx, "definitely-not-here"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat of a missing key = %v, want ErrNotFound", err)
	}
	if _, err := store.Open(ctx, "definitely-not-here"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open of a missing key = %v, want ErrNotFound", err)
	}
	// Delete is idempotent on purpose: GC re-running over an already-collected
	// key must not fail.
	if err := store.Delete(ctx, "definitely-not-here"); err != nil {
		t.Errorf("Delete of a missing key = %v, want nil", err)
	}
}

// Walk is what fsck compares the bucket against, and its two properties both
// have to hold on a real server: it must page past the 1000-key default, and it
// must strip the prefix so the keys it yields are the keys the database holds.
func TestLiveS3WalkPagesAndStripsThePrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 1200 objects")
	}
	store := liveStore(t)
	ctx := context.Background()

	// Just over one page, which is where a single-request implementation stops
	// and silently reports a partial bucket — the exact failure that would make
	// fsck call live content orphaned.
	const count = 1200
	written := make(map[string]bool, count)
	for i := 0; i < count; i++ {
		key := "walk" + itoa(i)
		if _, err := store.PutKeyed(ctx, key, strings.NewReader("x")); err != nil {
			t.Fatalf("writing %s: %v", key, err)
		}
		written[key] = true
	}

	seen := map[string]bool{}
	if err := store.Walk(ctx, func(key string, size int64) error {
		if strings.Contains(key, "livetest/") {
			t.Errorf("Walk yielded %q with the prefix still attached", key)
		}
		seen[key] = true
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	for key := range written {
		if !seen[key] {
			t.Fatalf("Walk missed %q — it stopped before the end of the bucket", key)
		}
	}
}

// itoa avoids importing strconv for one call in a test file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
