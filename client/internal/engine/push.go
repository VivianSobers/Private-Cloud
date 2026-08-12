package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/api"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/state"
)

// push scans the local tree and propagates what changed to the server: new
// folders and files created, existing files re-uploaded when their bytes moved,
// and — for recorded nodes no longer on disk — deletes.
//
// Everything is judged against the state database, so a change the pull just
// applied is recognized as already-synced and is not pushed back. That is what
// keeps the two loops from fighting each other into an upload/download cycle.
func (e *Engine) push(ctx context.Context) error {
	seen, err := e.pushExisting(ctx)
	if err != nil {
		return err
	}
	return e.pushDeletions(ctx, seen)
}

// pushExisting walks the disk, creating and uploading, and returns the set of
// server paths that currently exist locally.
func (e *Engine) pushExisting(ctx context.Context) (map[string]bool, error) {
	seen := map[string]bool{"/": true}

	err := filepath.WalkDir(e.root, func(abs string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A file that vanished mid-walk is handled by deletion detection, not
			// here; do not abort the whole scan for it.
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		// The state directory (database, WAL, download temp files) is never synced.
		if abs == e.stateDir {
			return filepath.SkipDir
		}
		sp, ok := e.serverPath(abs)
		if !ok || sp == "/" {
			return nil
		}
		// A selectively-excluded path is not pushed. Skipping the whole subtree of an
		// excluded directory keeps a locally-created file inside it off the server —
		// exclusion means "this device does not sync here", in both directions.
		if e.excluded(sp) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Ignored local paths (build output, editor scratch, OS metadata) are never
		// uploaded — the whole point of .pcsyncignore.
		if e.ignored(sp, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		seen[sp] = true

		if d.IsDir() {
			return e.pushFolder(ctx, sp)
		}
		// Skip anything that is not a regular file — symlinks, sockets, devices have
		// no meaning on the server and following them is how a sync loop escapes its
		// own root.
		if !d.Type().IsRegular() {
			delete(seen, sp)
			return nil
		}
		return e.pushFile(ctx, sp)
	})
	return seen, err
}

// pushFolder creates a folder on the server if it is not already recorded.
func (e *Engine) pushFolder(ctx context.Context, sp string) error {
	if _, ok, err := e.state.Get(sp); err != nil || ok {
		return err
	}
	parentID, err := e.parentNodeID(sp)
	if err != nil {
		return err
	}
	node, err := e.srv.CreateFolder(ctx, parentID, baseName(sp))
	if err != nil {
		// A concurrent pull may have created it; treat an existing name as success
		// and let the next pull record the server's node.
		if api.IsNotFound(err) {
			return nil
		}
		return err
	}
	e.log.Info("created folder", "path", sp)
	return e.state.Put(state.Entry{Path: sp, NodeID: node.ID, Kind: "folder"})
}

// pushFile uploads a new or locally-changed file.
func (e *Engine) pushFile(ctx context.Context, sp string) error {
	entry, ok, err := e.state.Get(sp)
	if err != nil {
		return err
	}
	if ok {
		exists, changed, err := e.localStatus(entry)
		if err != nil {
			return err
		}
		if !exists || !changed {
			return nil // unchanged since last sync, nothing to push
		}
	}

	parentID, err := e.parentNodeID(sp)
	if err != nil {
		return err
	}
	node, localHash, err := e.uploadFile(ctx, sp, parentID)
	if err != nil {
		return err
	}
	info, err := os.Stat(e.localPath(sp))
	if err != nil {
		return err
	}
	e.log.Info("pushed", "path", sp, "size", info.Size())
	e.countPush(info.Size())
	return e.state.Put(state.Entry{
		Path: sp, NodeID: node.ID, Kind: "file",
		Size: info.Size(), MtimeUnix: info.ModTime().Unix(),
		Hash: localHash, RemoteHash: node.ContentHash(),
	})
}

// pushDeletions trashes server nodes whose local counterpart is gone.
func (e *Engine) pushDeletions(ctx context.Context, seen map[string]bool) error {
	entries, err := e.state.List()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if seen[entry.Path] {
			continue
		}
		// An excluded entry is intentionally not on this device; its absence from the
		// walk must never trash it on the server. pruneExcluded drops its state, but
		// guard here too so a deletion is never sent for an excluded path.
		if e.excluded(entry.Path) {
			continue
		}
		// A newly-ignored path must not be trashed on the server just because it is
		// no longer walked locally — same guard as an excluded entry.
		if e.ignored(entry.Path, entry.Kind == "folder") {
			continue
		}
		// The parent folder may already have been trashed this pass, cascading on
		// the server; a 404 then just confirms the node is gone.
		if err := e.srv.Trash(ctx, entry.NodeID); err != nil && !api.IsNotFound(err) {
			return err
		}
		e.log.Info("trashed", "path", entry.Path)
		if err := e.state.Delete(entry.Path); err != nil {
			return err
		}
	}
	return nil
}

// parentNodeID resolves the server node id of a path's parent folder — the root
// for a top-level node, otherwise a folder the scan has already recorded.
func (e *Engine) parentNodeID(sp string) (string, error) {
	parent := parentPath(sp)
	if parent == "/" {
		return e.rootID, nil
	}
	entry, ok, err := e.state.Get(parent)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", &missingParentError{path: parent}
	}
	return entry.NodeID, nil
}

type missingParentError struct{ path string }

func (m *missingParentError) Error() string { return "no recorded parent folder for " + m.path }
