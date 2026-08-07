package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/state"
)

// countStateUnder returns how many recorded entries fall under a prefix.
func countStateUnder(t *testing.T, e *Engine, prefix string) int {
	t.Helper()
	entries, err := e.state.List()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range entries {
		if isUnder(entry.Path, prefix) {
			n++
		}
	}
	return n
}

// An excluded subtree is never downloaded on an initial sync: the rest of the
// tree lands, the excluded folder does not, and nothing under it is tracked.
func TestExcludeSkipsInitialDownload(t *testing.T) {
	f := newFake()
	f.seedWhole(t, "/keep.txt", []byte("hello"))
	f.seedFolder(t, "/Videos")
	f.seedChunked(t, "/Videos/big.bin", bigContent("v"))

	e, root := newTestEngine(t, f)
	e.SetExcludes([]string{"/Videos"})
	if err := e.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Errorf("included file not synced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Videos")); !os.IsNotExist(err) {
		t.Errorf("excluded folder was materialized: err=%v", err)
	}
	if n := countStateUnder(t, e, "/Videos"); n != 0 {
		t.Errorf("excluded subtree left %d state entries, want 0", n)
	}
}

// Excluding an already-synced clean subtree prunes it locally to reclaim space,
// but never deletes it on the server — each device keeps its own subset.
func TestExcludePrunesLocallyButKeepsServer(t *testing.T) {
	f := newFake()
	f.seedFolder(t, "/Videos")
	f.seedChunked(t, "/Videos/big.bin", bigContent("v"))
	e, root := newTestEngine(t, f)
	ctx := context.Background()

	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Videos", "big.bin")); err != nil {
		t.Fatalf("precondition: file should be synced first: %v", err)
	}

	// Now exclude it and reconcile.
	e.SetExcludes([]string{"/Videos"})
	if err := e.Sync(ctx); err != nil {
		t.Fatalf("sync after exclude: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "Videos")); !os.IsNotExist(err) {
		t.Errorf("excluded subtree not pruned locally: err=%v", err)
	}
	if n := countStateUnder(t, e, "/Videos"); n != 0 {
		t.Errorf("excluded subtree still tracked: %d entries", n)
	}
	// The server still has the file — exclusion is a local decision.
	if _, ok := f.content("/Videos/big.bin"); !ok {
		t.Error("exclusion deleted the file on the server")
	}
}

// A local edit inside a newly-excluded subtree is never destroyed: the subtree is
// left on disk (it stops syncing) rather than pruned, and the server is untouched.
func TestExcludePreservesLocalEdits(t *testing.T) {
	f := newFake()
	f.seedFolder(t, "/Videos")
	f.seedWhole(t, "/Videos/note.txt", []byte("original"))
	e, root := newTestEngine(t, f)
	ctx := context.Background()
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	edited := filepath.Join(root, "Videos", "note.txt")
	if err := os.WriteFile(edited, []byte("MY LOCAL EDIT"), 0o644); err != nil {
		t.Fatal(err)
	}

	e.SetExcludes([]string{"/Videos"})
	if err := e.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// The edited file survives on disk with its edit intact...
	got, err := os.ReadFile(edited)
	if err != nil {
		t.Fatalf("local edit was destroyed: %v", err)
	}
	if !bytes.Equal(got, []byte("MY LOCAL EDIT")) {
		t.Errorf("local edit content = %q", got)
	}
	// ...it is no longer tracked, and the server keeps its own version.
	if n := countStateUnder(t, e, "/Videos"); n != 0 {
		t.Errorf("kept subtree still tracked: %d entries", n)
	}
	if body, ok := f.content("/Videos/note.txt"); !ok || !bytes.Equal(body, []byte("original")) {
		t.Errorf("server version disturbed: ok=%v body=%q", ok, body)
	}
}

// A file created locally inside an excluded subtree is not pushed to the server —
// exclusion means "this device does not sync here", in both directions.
func TestExcludeBlocksLocalCreationPush(t *testing.T) {
	f := newFake()
	e, root := newTestEngine(t, f)
	e.SetExcludes([]string{"/scratch"})

	if err := os.MkdirAll(filepath.Join(root, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scratch", "local.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, ok := f.content("/scratch/local.txt"); ok {
		t.Error("a file in an excluded folder was pushed to the server")
	}
}

// normalizeExcludes rewrites raw entries into rooted, trimmed, de-duplicated,
// sorted prefixes, and drops the whole-tree root.
func TestNormalizeExcludes(t *testing.T) {
	got := normalizeExcludes([]string{"Videos/", "/Photos", "  ", "/", "Videos", "/Archive/"})
	want := []string{"/Archive", "/Photos", "/Videos"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalizeExcludes = %v, want %v", got, want)
	}
}

// A persisted exclude set survives a restart and is not clobbered by the config
// seed; a fresh database, by contrast, takes the seed.
func TestSeedRespectsPersistedExcludes(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".pcsync")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(stateDir, "state.db")
	f := newFake()

	// First run: no persisted set, so the config seed applies and persists.
	st1, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	e1 := New(f, st1, root, stateDir, nil)
	e1.SeedExcludes([]string{"/FromConfig"})
	if got := e1.Excludes(); !reflect.DeepEqual(got, []string{"/FromConfig"}) {
		t.Fatalf("first-run excludes = %v", got)
	}
	// A live change through the control surface.
	e1.SetExcludes([]string{"/LiveChoice"})
	st1.Close()

	// Second run on the same database: the persisted live choice loads, and a
	// different config seed does NOT override it.
	st2, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	e2 := New(f, st2, root, stateDir, nil)
	if got := e2.Excludes(); !reflect.DeepEqual(got, []string{"/LiveChoice"}) {
		t.Errorf("persisted excludes not loaded on restart: %v", got)
	}
	e2.SeedExcludes([]string{"/FromConfig"})
	if got := e2.Excludes(); !reflect.DeepEqual(got, []string{"/LiveChoice"}) {
		t.Errorf("config seed clobbered a persisted set: %v", got)
	}
}
