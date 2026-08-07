package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/api"
)

// failRoot wraps a fake server but always fails the very first call a reconcile
// makes (GetRoot), so syncTracked returns an error without needing the network.
type failRoot struct{ *fakeServer }

func (failRoot) GetRoot(context.Context) (api.Node, error) {
	return api.Node{}, errors.New("server unreachable")
}

// A successful reconcile leaves the snapshot idle, error-free, stamped with a
// last-sync time, and counting what it tracked.
func TestSnapshotAfterSuccessfulSync(t *testing.T) {
	f := newFake()
	f.seedWhole(t, "/note.txt", []byte("hello"))
	f.seedFolder(t, "/docs")
	e, _ := newTestEngine(t, f)

	// Before the first sync: idle, never synced, nothing tracked.
	if s := e.Snapshot(); s.Phase != PhaseIdle || !s.LastSync.IsZero() || s.Tracked != 0 {
		t.Fatalf("pre-sync snapshot = %+v", s)
	}

	if err := e.syncTracked(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	s := e.Snapshot()
	if s.Phase != PhaseIdle {
		t.Errorf("phase = %q, want idle", s.Phase)
	}
	if s.LastError != "" || !s.LastErrorAt.IsZero() {
		t.Errorf("unexpected error recorded: %q at %v", s.LastError, s.LastErrorAt)
	}
	if s.LastSync.IsZero() {
		t.Error("last-sync time not stamped after a successful reconcile")
	}
	if s.Tracked < 2 {
		t.Errorf("tracked = %d, want at least the folder and the file", s.Tracked)
	}
}

// A failed reconcile leaves the snapshot in error with the message, and a later
// success clears it — the icon shows a lingering problem, then recovers.
func TestSnapshotRecordsAndClearsError(t *testing.T) {
	f := newFake()
	f.seedWhole(t, "/note.txt", []byte("hello"))
	e, _ := newTestEngine(t, f)
	ctx := context.Background()

	// Point the engine at a server whose first call fails.
	e.srv = failRoot{f}
	if err := e.syncTracked(ctx); err == nil {
		t.Fatal("expected the reconcile to fail")
	}
	if s := e.Snapshot(); s.Phase != PhaseError || s.LastError == "" || s.LastErrorAt.IsZero() {
		t.Fatalf("error snapshot = %+v", s)
	}

	// The same engine, now with a working server, recovers on the next run.
	e.srv = f
	if err := e.syncTracked(ctx); err != nil {
		t.Fatalf("recovery sync: %v", err)
	}
	if s := e.Snapshot(); s.Phase != PhaseIdle || s.LastError != "" {
		t.Errorf("error not cleared after a successful run: %+v", s)
	}
}

// The session transfer tallies accrue: a pull counts downloaded files and bytes,
// a subsequent local creation counts an upload.
func TestTransferCountsAccrue(t *testing.T) {
	f := newFake()
	f.seedWhole(t, "/down.txt", []byte("downloaded body"))
	e, root := newTestEngine(t, f)
	ctx := context.Background()

	if err := e.syncTracked(ctx); err != nil {
		t.Fatal(err)
	}
	s := e.Snapshot()
	if s.PulledFiles < 1 || s.PulledBytes < 1 {
		t.Errorf("pull not counted: files=%d bytes=%d", s.PulledFiles, s.PulledBytes)
	}

	// Create a local file and sync again: it should be pushed and counted.
	if err := os.WriteFile(filepath.Join(root, "up.txt"), []byte("uploaded"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.syncTracked(ctx); err != nil {
		t.Fatal(err)
	}
	if s := e.Snapshot(); s.PushedFiles < 1 || s.PushedBytes < 1 {
		t.Errorf("push not counted: files=%d bytes=%d", s.PushedFiles, s.PushedBytes)
	}
}

// Pause is reflected in the snapshot and is independent of the phase; SyncNow is
// non-blocking and coalesces repeats into a single pending trigger.
func TestPauseAndSyncNowControls(t *testing.T) {
	f := newFake()
	e, _ := newTestEngine(t, f)

	if e.Paused() || e.Snapshot().Paused {
		t.Fatal("engine should start unpaused")
	}
	e.Pause()
	if !e.Paused() || !e.Snapshot().Paused {
		t.Error("pause not reflected")
	}
	e.Resume()
	if e.Paused() || e.Snapshot().Paused {
		t.Error("resume not reflected")
	}

	// Two SyncNow calls never block and leave exactly one pending trigger.
	e.SyncNow()
	e.SyncNow()
	select {
	case <-e.syncNow:
	default:
		t.Fatal("SyncNow left no pending trigger")
	}
	select {
	case <-e.syncNow:
		t.Fatal("SyncNow left more than one pending trigger")
	default:
	}
}

// A conflict copy is recorded in the snapshot for the "needs your attention"
// list, naming the original and the copy.
func TestConflictRecordedInSnapshot(t *testing.T) {
	f := newFake()
	f.seedWhole(t, "/c.txt", []byte("base"))
	e, root := newTestEngine(t, f)
	pinConflictNaming(e)
	ctx := context.Background()
	if err := e.syncTracked(ctx); err != nil {
		t.Fatal(err)
	}

	f.seedWhole(t, "/c.txt", []byte("REMOTE VERSION"))
	if err := os.WriteFile(filepath.Join(root, "c.txt"), []byte("LOCAL EDIT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.syncTracked(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	conflicts := e.Snapshot().Conflicts
	if len(conflicts) != 1 {
		t.Fatalf("recorded %d conflicts, want 1", len(conflicts))
	}
	if conflicts[0].Original != "/c.txt" {
		t.Errorf("conflict original = %q", conflicts[0].Original)
	}
	if conflicts[0].Copy != "/c"+conflictSuffix+".txt" {
		t.Errorf("conflict copy = %q", conflicts[0].Copy)
	}
	if conflicts[0].At.IsZero() {
		t.Error("conflict timestamp not set")
	}

	// Clearing dismisses the log without touching the files on disk.
	e.ClearConflicts()
	if got := e.Snapshot().Conflicts; len(got) != 0 {
		t.Errorf("conflicts not cleared: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "c"+conflictSuffix+".txt")); err != nil {
		t.Errorf("clearing the log deleted the conflict copy on disk: %v", err)
	}
}
