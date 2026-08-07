package control

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/engine"
)

// stubEngine records the control actions and returns a canned snapshot, so the
// server and client can be exercised without a real sync loop.
type stubEngine struct {
	mu       sync.Mutex
	paused   bool
	syncs    int
	snapshot engine.Status
}

func (s *stubEngine) Snapshot() engine.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snapshot
	snap.Paused = s.paused
	return snap
}
func (s *stubEngine) Pause()  { s.mu.Lock(); s.paused = true; s.mu.Unlock() }
func (s *stubEngine) Resume() { s.mu.Lock(); s.paused = false; s.mu.Unlock() }
func (s *stubEngine) SyncNow() {
	s.mu.Lock()
	s.syncs++
	s.mu.Unlock()
}

// serveOnSocket starts a control server on a temp socket and returns a client
// wired to it, tearing both down at test end.
func serveOnSocket(t *testing.T, eng Engine, info Info) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), SocketName)
	srv := NewServer(eng, info, nil)
	go func() { _ = srv.Serve(sock) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	client := NewClient(sock)
	// Wait for the listener to come up so the first call doesn't race the goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := client.Status(ctx)
		cancel()
		if err == nil {
			return client
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("control server never became reachable")
	return nil
}

// The full round trip: a client reads status and drives pause/resume/sync over a
// real Unix socket, and the daemon-side engine sees each action.
func TestControlRoundTrip(t *testing.T) {
	eng := &stubEngine{snapshot: engine.Status{
		Phase:     engine.PhaseIdle,
		LastSync:  time.Now(),
		Tracked:   7,
		Conflicts: []engine.ConflictRecord{{Original: "/a.txt", Copy: "/a (conflict).txt", At: time.Now()}},
	}}
	client := serveOnSocket(t, eng, Info{Server: "https://cloud.example.ts.net", Root: "/home/u/Cloud", Version: "test"})
	ctx := context.Background()

	// Status carries both the engine snapshot and the static info.
	st, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Phase != engine.PhaseIdle || st.Tracked != 7 {
		t.Errorf("status snapshot wrong: %+v", st)
	}
	if st.Server != "https://cloud.example.ts.net" || st.Root != "/home/u/Cloud" || st.Version != "test" {
		t.Errorf("status static info wrong: %+v", st)
	}

	// Conflicts come back as a populated list.
	conflicts, err := client.Conflicts(ctx)
	if err != nil {
		t.Fatalf("conflicts: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].Original != "/a.txt" {
		t.Errorf("conflicts = %+v", conflicts)
	}

	// Pause / resume / sync reach the engine.
	if err := client.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if st, _ := client.Status(ctx); !st.Paused {
		t.Error("pause not reflected in status")
	}
	if err := client.Resume(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if st, _ := client.Status(ctx); st.Paused {
		t.Error("resume not reflected in status")
	}
	if err := client.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	eng.mu.Lock()
	syncs := eng.syncs
	eng.mu.Unlock()
	if syncs != 1 {
		t.Errorf("engine saw %d sync requests, want 1", syncs)
	}
}

// A GET to a POST-only control is a 405, not a silent success — a fat-fingered
// verb fails loudly.
func TestWrongMethodRejected(t *testing.T) {
	client := serveOnSocket(t, &stubEngine{}, Info{})
	// Status uses GET; hitting the pause path with GET must fail.
	err := client.do(context.Background(), "GET", "/v1/pause", nil)
	if err == nil {
		t.Fatal("expected GET on a POST-only route to fail")
	}
}
