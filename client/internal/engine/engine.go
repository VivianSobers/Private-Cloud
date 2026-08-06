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

	rootID string // server root node id, cached per Sync
}

// New builds an engine. root and stateDir are absolute; stateDir is where the
// state database and download temp files live, and it is never synced.
func New(srv Server, st *state.Store, root, stateDir string, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Engine{srv: srv, state: st, root: root, stateDir: stateDir, log: log}
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
