package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/state"
)

func newTestEngine(t *testing.T, f *fakeServer) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, ".pcsync")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(f, st, root, stateDir, nil), root
}

// bigContent is deterministic content large enough to chunk (well above the 2 KiB
// whole-file threshold), so tests that want the delta path get it.
func bigContent(tag string) []byte {
	return append([]byte(tag+":"), bytes.Repeat([]byte("sync-engine-block "), 1500)...)
}

func readLocal(t *testing.T, root, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read local %s: %v", rel, err)
	}
	return data
}

// Initial sync materializes the whole server tree locally: folders as
// directories, whole-file blobs and chunked files alike as their exact bytes.
func TestInitialSyncMaterializes(t *testing.T) {
	f := newFake()
	f.seedFolder(t, "/docs")
	f.seedWhole(t, "/note.txt", []byte("hello"))
	big := bigContent("big")
	f.seedChunked(t, "/docs/big.bin", big)

	e, root := newTestEngine(t, f)
	if err := e.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if info, err := os.Stat(filepath.Join(root, "docs")); err != nil || !info.IsDir() {
		t.Errorf("docs not a directory: err=%v", err)
	}
	if got := readLocal(t, root, "note.txt"); !bytes.Equal(got, []byte("hello")) {
		t.Errorf("note.txt = %q", got)
	}
	if got := readLocal(t, root, "docs/big.bin"); !bytes.Equal(got, big) {
		t.Errorf("big.bin did not round-trip (%d bytes)", len(got))
	}
}

// A file created locally is pushed to the server, via the delta path for large
// files and the whole path for small ones.
func TestPushNewFiles(t *testing.T) {
	f := newFake()
	e, root := newTestEngine(t, f)
	ctx := context.Background()
	if err := e.Sync(ctx); err != nil { // establishes the cursor on an empty tree
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := bigContent("data")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "data.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if got, ok := f.content("/hello.txt"); !ok || !bytes.Equal(got, []byte("world")) {
		t.Errorf("hello.txt not pushed: ok=%v got=%q", ok, got)
	}
	if got, ok := f.content("/sub/data.bin"); !ok || !bytes.Equal(got, big) {
		t.Errorf("data.bin not pushed via delta path: ok=%v len=%d", ok, len(got))
	}
}

// A new server version of a file is pulled down and replaces the local copy.
func TestApplyRemoteEdit(t *testing.T) {
	f := newFake()
	f.seedWhole(t, "/a.txt", []byte("v1"))
	e, root := newTestEngine(t, f)
	ctx := context.Background()
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	f.seedWhole(t, "/a.txt", []byte("v2 is longer")) // a new version on the server
	if err := e.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := readLocal(t, root, "a.txt"); !bytes.Equal(got, []byte("v2 is longer")) {
		t.Errorf("remote edit not applied: %q", got)
	}
}

// Deleting a file locally trashes it on the server.
func TestLocalDeletePropagates(t *testing.T) {
	f := newFake()
	f.seedWhole(t, "/gone.txt", []byte("x"))
	e, root := newTestEngine(t, f)
	ctx := context.Background()
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !f.isTrashed("/gone.txt") {
		t.Error("local delete did not trash the server node")
	}
}

// A remote delete removes the local file.
func TestRemoteDeletePropagates(t *testing.T) {
	f := newFake()
	f.seedWhole(t, "/r.txt", []byte("y"))
	e, root := newTestEngine(t, f)
	ctx := context.Background()
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// Trash it on the server.
	id := f.liveByPath("/r.txt").id
	if err := f.Trash(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "r.txt")); !os.IsNotExist(err) {
		t.Errorf("remote delete did not remove the local file: err=%v", err)
	}
}

// When a file changed on both sides, slice 3 must not lose the local edit: the
// pull declines to overwrite it, and the push uploads it as the new head. The
// server keeps the remote edit in version history.
func TestBothChangedKeepsLocalEdit(t *testing.T) {
	f := newFake()
	f.seedWhole(t, "/c.txt", []byte("base"))
	e, root := newTestEngine(t, f)
	ctx := context.Background()
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// Both sides move independently.
	f.seedWhole(t, "/c.txt", []byte("REMOTE VERSION"))
	if err := os.WriteFile(filepath.Join(root, "c.txt"), []byte("LOCAL EDIT WINS"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := e.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := readLocal(t, root, "c.txt"); !bytes.Equal(got, []byte("LOCAL EDIT WINS")) {
		t.Errorf("local edit was overwritten by the remote version: %q", got)
	}
	if got, _ := f.content("/c.txt"); !bytes.Equal(got, []byte("LOCAL EDIT WINS")) {
		t.Errorf("local edit was not pushed as the new head: %q", got)
	}
}
