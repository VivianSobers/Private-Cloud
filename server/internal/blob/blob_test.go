package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *FSStore {
	t.Helper()
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	return s
}

func TestPutOpenRoundTrip(t *testing.T) {
	s := newStore(t)
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

	// Two levels of fan-out: a single flat directory with a million entries is
	// slow to list and unpleasant to inspect by hand.
	if parts := strings.Split(res.Key, "/"); len(parts) != 3 {
		t.Errorf("key %q should be ab/cd/abcd...", res.Key)
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

func TestOpenIsSeekable(t *testing.T) {
	// Range requests seek rather than discarding a prefix. Without this,
	// scrubbing a video re-reads the file from the start on every seek.
	s := newStore(t)
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
}

func TestPutEmpty(t *testing.T) {
	// A zero-byte file is legitimate and must round-trip; an empty upload that
	// errors would be a surprising way to lose a `touch`ed file.
	s := newStore(t)
	res, err := s.Put(context.Background(), bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("Put(empty): %v", err)
	}
	if res.Size != 0 {
		t.Errorf("Size = %d, want 0", res.Size)
	}
	if n, err := s.Stat(context.Background(), res.Key); err != nil || n != 0 {
		t.Errorf("Stat = %d, %v; want 0, nil", n, err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	// GC retries must not turn into permanent failures.
	s := newStore(t)
	ctx := context.Background()

	res, err := s.Put(ctx, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, res.Key); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := s.Delete(ctx, res.Key); err != nil {
		t.Fatalf("second Delete should be a no-op, got %v", err)
	}
	if _, err := s.Open(ctx, res.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open after Delete = %v, want ErrNotFound", err)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	// Keys are server-generated today. This is the boundary where a traversal
	// would land if that ever stopped being true.
	s := newStore(t)
	ctx := context.Background()

	bad := []string{
		"",
		"../etc/passwd",
		"ab/../../etc/passwd",
		"/etc/passwd",
		`ab\cd\ef`,
		"C:/windows",
	}
	for _, key := range bad {
		if _, err := s.Open(ctx, key); err == nil {
			t.Errorf("Open(%q) succeeded, want rejection", key)
		}
		if err := s.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) succeeded, want rejection", key)
		}
	}
}

func TestWalkSkipsTempAndProbeFiles(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	res, err := s.Put(ctx, strings.NewReader("real content"))
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a crash mid-upload.
	dir := filepath.Join(s.Root(), "zz", "zz")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".upload-abcdef")
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	var seen []string
	if err := s.Walk(func(key string, _ int64) error {
		seen = append(seen, key)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(seen) != 1 || seen[0] != res.Key {
		t.Errorf("Walk returned %v, want just %q — temp files are not blobs", seen, res.Key)
	}

	n, err := s.SweepTempFiles()
	if err != nil {
		t.Fatalf("SweepTempFiles: %v", err)
	}
	if n != 1 {
		t.Errorf("SweepTempFiles removed %d, want 1", n)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("temp file survived the sweep")
	}
}

func TestPutAbortsOnCancelledContext(t *testing.T) {
	// A client that disconnects mid-upload must not leave the server writing to
	// disk for as long as the body would have taken.
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Put(ctx, strings.NewReader("never written")); err == nil {
		t.Fatal("Put with a cancelled context succeeded, want error")
	}

	// And it must not leave debris behind.
	var found int
	_ = s.Walk(func(string, int64) error { found++; return nil })
	if found != 0 {
		t.Errorf("aborted Put left %d blob(s) behind", found)
	}
}

func TestNewFSStoreRejectsEmptyRoot(t *testing.T) {
	if _, err := NewFSStore(""); err == nil {
		t.Error("NewFSStore(\"\") succeeded; an empty root would write into the working directory")
	}
}
