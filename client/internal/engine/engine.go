// Package engine is the sync client's reconciliation core: it converges a local
// folder with the server's tree in both directions, judging every change against
// the local state database rather than the filesystem clock.
//
// The server stays authoritative. The engine never talks to another client; it
// pulls remote changes from the change journal and pushes local ones through the
// delta protocol, and two devices converge because they both converge on the
// server. That is what keeps sync a two-party problem instead of an n-party
// consensus one.
package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/api"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/state"
)

// Server is the subset of the API the engine drives. An interface, not the
// concrete client, so the reconciliation logic can be tested against an
// in-memory server with no network or database.
type Server interface {
	GetRoot(ctx context.Context) (api.Node, error)
	ListChildren(ctx context.Context, nodeID string) ([]api.Node, error)
	CreateFolder(ctx context.Context, parentID, name string) (api.Node, error)
	Upload(ctx context.Context, parentID, name string, r io.Reader) (api.Node, error)
	Download(ctx context.Context, nodeID string) (io.ReadCloser, error)
	Manifest(ctx context.Context, nodeID string) (api.Manifest, error)
	HaveChunks(ctx context.Context, hashes []string) ([]string, error)
	PutChunk(ctx context.Context, hash string, plain []byte) error
	GetChunk(ctx context.Context, hash string) ([]byte, error)
	CommitManifest(ctx context.Context, parentID, name, contentHash string, chunks []string, mime string) (api.Node, error)
	Trash(ctx context.Context, nodeID string) error
	Move(ctx context.Context, nodeID, name, parentID string) (api.Node, error)
	Poll(ctx context.Context, since int64, limit int) (api.Changes, error)
}

// changePageLimit bounds one journal page.
const changePageLimit = 500

// Engine reconciles one local root against one server account.
type Engine struct {
	srv      Server
	state    *state.Store
	root     string // absolute local root
	stateDir string // absolute; excluded from the synced tree
	log      *slog.Logger

	// hostname and clock name conflict copies. They are fields, not global calls,
	// so a test can make the name deterministic.
	hostname string
	clock    func() time.Time

	rootID string // server root node id, cached per Sync

	// Observation and control, read/written from the control goroutine while the
	// sync loop runs — see status.go. mu guards the status fields; paused and
	// syncNow steer the Run loop from outside it.
	mu        sync.Mutex
	phase     Phase
	lastSync  time.Time
	lastErr   string
	lastErrAt time.Time
	conflicts []ConflictRecord
	since     time.Time
	paused    atomic.Bool
	syncNow   chan struct{}

	// excludes is the selective-sync set (*[]string of normalized prefixes), read
	// per node on the sync path and swapped live from the control goroutine — see
	// selective.go. excludesLoaded records whether a persisted set was found at
	// startup, so the config seed does not clobber a live change.
	excludes       atomic.Pointer[[]string]
	excludesLoaded bool

	// Session transfer tallies since the daemon started, incremented on the sync
	// path and read for the status surface — atomic, so no lock is taken per file.
	pulledFiles atomic.Int64
	pushedFiles atomic.Int64
	pulledBytes atomic.Int64
	pushedBytes atomic.Int64
}

// New builds an engine. root and stateDir are absolute; stateDir is where the
// state database and download temp files live, and it is never synced.
func New(srv Server, st *state.Store, root, stateDir string, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "device"
	}
	e := &Engine{
		srv: srv, state: st, root: root, stateDir: stateDir, log: log,
		hostname: host, clock: time.Now,
		phase: PhaseIdle, since: time.Now(),
		// Buffered so SyncNow never blocks the caller: a trigger already waiting is
		// coalesced with a new one — one pending sync is as good as two.
		syncNow: make(chan struct{}, 1),
	}
	e.loadPersistedExcludes()
	return e
}

// Sync runs one full reconciliation: pull remote changes, then push local ones.
//
// Pull before push, deliberately: applying the server's state first means a local
// scan pushes against an up-to-date baseline, so a file the server already changed
// is not re-uploaded from a stale local copy. Where both sides moved, the pull
// declines to overwrite the local edit and the push uploads it as a new version —
// the server keeps the remote edit in history, so nothing is lost even before
// slice 4 turns that into a visible conflict copy.
func (e *Engine) Sync(ctx context.Context) error {
	if err := e.ensureRoot(ctx); err != nil {
		return err
	}
	// Drop anything newly excluded before reconciling, so the pull does not
	// re-download what selective sync just declined, and the push does not read a
	// pruned subtree's absence as a server deletion.
	if err := e.pruneExcluded(); err != nil {
		return err
	}
	if err := e.pull(ctx); err != nil {
		return err
	}
	return e.push(ctx)
}

// ensureRoot resolves and caches the server root id, and makes sure the local
// root and state directory exist.
func (e *Engine) ensureRoot(ctx context.Context) error {
	if err := os.MkdirAll(e.root, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		return err
	}
	root, err := e.srv.GetRoot(ctx)
	if err != nil {
		return err
	}
	e.rootID = root.ID
	return nil
}

// pull applies remote changes: a full tree reconcile when nothing has ever been
// synced (or after a reset), otherwise an incremental replay of the journal.
func (e *Engine) pull(ctx context.Context) error {
	empty, err := e.state.Empty()
	if err != nil {
		return err
	}
	if empty {
		latest, err := e.latestSeq(ctx)
		if err != nil {
			return err
		}
		if err := e.reconcileTree(ctx); err != nil {
			return err
		}
		// The cursor is the journal head captured BEFORE the walk, so any change
		// that landed during it is replayed next pull rather than skipped.
		return e.state.SetCursor(latest)
	}
	return e.applyRemote(ctx)
}

// latestSeq reads the journal head without consuming any entries.
func (e *Engine) latestSeq(ctx context.Context) (int64, error) {
	resp, err := e.srv.Poll(ctx, 0, 1)
	if err != nil {
		return 0, err
	}
	return resp.Latest, nil
}
