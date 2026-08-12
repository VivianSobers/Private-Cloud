package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeIgnore drops a .pcsyncignore in the sync root.
func writeIgnore(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".pcsyncignore"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Files matching .pcsyncignore are never uploaded, and an ignored directory's
// whole subtree is skipped — the server never sees local junk.
func TestIgnoreBlocksLocalUpload(t *testing.T) {
	f := newFake()
	e, root := newTestEngine(t, f)
	writeIgnore(t, root, "# junk\n*.tmp\nbuild/\n")

	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(root, "keep.txt"), []byte("real"), 0o644))
	must(os.WriteFile(filepath.Join(root, "scratch.tmp"), []byte("junk"), 0o644))
	must(os.MkdirAll(filepath.Join(root, "build"), 0o755))
	must(os.WriteFile(filepath.Join(root, "build", "out.o"), []byte("object"), 0o644))

	if err := e.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if _, ok := f.content("/keep.txt"); !ok {
		t.Error("a normal file was not synced")
	}
	if _, ok := f.content("/scratch.tmp"); ok {
		t.Error("an ignored *.tmp file was uploaded")
	}
	if _, ok := f.content("/build/out.o"); ok {
		t.Error("a file under an ignored directory was uploaded")
	}
}

// A server file matching an ignore rule is not downloaded, so a device stays free
// of a class of file even if another device uploaded it.
func TestIgnoreBlocksDownload(t *testing.T) {
	f := newFake()
	f.seedWhole(t, "/keep.txt", []byte("real"))
	f.seedWhole(t, "/remote.tmp", []byte("junk from another device"))
	e, root := newTestEngine(t, f)
	writeIgnore(t, root, "*.tmp\n")

	if err := e.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Errorf("normal file not materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "remote.tmp")); !os.IsNotExist(err) {
		t.Errorf("ignored server file was downloaded: err=%v", err)
	}
	if n := countStateUnder(t, e, "/remote.tmp"); n != 0 {
		t.Errorf("ignored file left %d state entries, want 0", n)
	}
}
