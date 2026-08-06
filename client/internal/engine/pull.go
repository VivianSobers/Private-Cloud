package engine

import (
	"context"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/zeebo/blake3"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/api"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/state"
)

// reconcileTree walks the entire server tree and makes local state match it. It
// is the initial sync when nothing has been synced, and the recovery path after a
// reset — the journal cursor predates retained history, so a full comparison is
// the only way to be sure nothing was missed.
func (e *Engine) reconcileTree(ctx context.Context) error {
	seen := map[string]bool{"/": true}

	// Breadth-first from the root, so a folder is materialized before the files
	// inside it and the parent id is always known.
	queue := []string{e.rootID}
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]

		children, err := e.srv.ListChildren(ctx, parentID)
		if err != nil {
			return err
		}
		for _, node := range children {
			seen[node.Path] = true
			if node.IsFolder() {
				if err := e.materializeFolder(node); err != nil {
					return err
				}
				queue = append(queue, node.ID)
				continue
			}
			if err := e.materializeFile(ctx, node); err != nil {
				return err
			}
		}
	}

	// Anything recorded but no longer on the server was removed remotely while we
	// were away. Drop it locally, unless the user has changed it since — a delete
	// that destroys an unseen local edit is data loss, so that case is left for
	// the local scan to push as a new version.
	entries, err := e.state.List()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if seen[entry.Path] {
			continue
		}
		if err := e.removeVanished(entry); err != nil {
			return err
		}
	}
	return nil
}

// materializeFolder ensures a folder exists locally and is recorded.
func (e *Engine) materializeFolder(node api.Node) error {
	if err := os.MkdirAll(e.localPath(node.Path), 0o755); err != nil {
		return err
	}
	return e.state.Put(state.Entry{Path: node.Path, NodeID: node.ID, Kind: "folder"})
}

// materializeFile downloads a file unless the local copy already matches the
// server's version, so a full reconcile does not re-transfer what is already in
// place.
func (e *Engine) materializeFile(ctx context.Context, node api.Node) error {
	entry, ok, err := e.state.Get(node.Path)
	if err != nil {
		return err
	}
	if ok && entry.RemoteHash == node.ContentHash() {
		exists, changed, err := e.localStatus(entry)
		if err != nil {
			return err
		}
		if exists && !changed {
			return nil // already have exactly this version, untouched
		}
	}
	return e.pullDown(ctx, node)
}

// applyRemote replays the journal from the saved cursor, applying each change.
func (e *Engine) applyRemote(ctx context.Context) error {
	cursor, err := e.state.Cursor()
	if err != nil {
		return err
	}
	for {
		resp, err := e.srv.Poll(ctx, cursor, changePageLimit)
		if err != nil {
			return err
		}
		if resp.Reset {
			// The cursor is older than the journal retains; a partial replay would
			// silently skip changes. Re-walk the whole tree and adopt the head.
			if err := e.reconcileTree(ctx); err != nil {
				return err
			}
			return e.state.SetCursor(resp.Latest)
		}
		for _, ch := range resp.Changes {
			if err := e.applyChange(ctx, ch); err != nil {
				return err
			}
		}
		cursor = resp.Cursor
		if err := e.state.SetCursor(cursor); err != nil {
			return err
		}
		if !resp.HasMore {
			return nil
		}
	}
}

// applyChange reconciles one journal entry into the local tree.
func (e *Engine) applyChange(ctx context.Context, ch api.Change) error {
	if ch.Kind == "delete" {
		return e.applyDelete(ch.NodeID)
	}
	// An upsert whose node is absent has been superseded by a later change in the
	// same stream — the journal is self-healing, so let that later entry correct
	// it rather than acting on a state we know is stale.
	if ch.Node == nil {
		return nil
	}
	node := *ch.Node

	// A rename or move: a node we already hold has surfaced at a new path. Move the
	// local copy rather than downloading a second one and orphaning the first.
	if prev, ok, err := e.state.GetByNodeID(node.ID); err != nil {
		return err
	} else if ok && prev.Path != node.Path {
		if err := e.moveLocal(prev, node.Path); err != nil {
			return err
		}
	}

	if node.IsFolder() {
		return e.materializeFolder(node)
	}
	return e.applyFileUpsert(ctx, node)
}

// applyFileUpsert brings the local file in line with a server file version,
// declining to overwrite an independent local edit.
func (e *Engine) applyFileUpsert(ctx context.Context, node api.Node) error {
	entry, ok, err := e.state.Get(node.Path)
	if err != nil {
		return err
	}
	if !ok {
		// New to us: download it.
		return e.pullDown(ctx, node)
	}

	exists, changed, err := e.localStatus(entry)
	if err != nil {
		return err
	}

	if entry.RemoteHash == node.ContentHash() {
		// The server's version has not moved since we synced. If the local file is
		// missing, restore it; otherwise leave a local edit alone for the push to
		// carry.
		if !exists {
			return e.pullDown(ctx, node)
		}
		return nil
	}

	// The server has a new version. Overwrite only when the local copy is the one
	// we last synced. A local edit that also changed the file is a conflict by
	// lineage — both sides moved past the recorded base — so the local edit is set
	// aside under a conflict name and the server's version takes the original name.
	// Nothing is overwritten, and the set-aside file is pushed as its own file.
	if exists && changed {
		if _, err := e.conflictCopy(entry); err != nil {
			return err
		}
	}
	return e.pullDown(ctx, node)
}

// pullDown downloads a node's content and records the new synced state.
func (e *Engine) pullDown(ctx context.Context, node api.Node) error {
	hash, size, mtime, err := e.downloadFile(ctx, node, node.Path)
	if err != nil {
		return err
	}
	e.log.Info("pulled", "path", node.Path, "size", size)
	return e.state.Put(state.Entry{
		Path: node.Path, NodeID: node.ID, Kind: "file",
		Size: size, MtimeUnix: mtime, Hash: hash, RemoteHash: node.ContentHash(),
	})
}

// applyDelete removes a node the server reports gone, sweeping a folder's whole
// subtree in one step so the per-descendant delete rows that follow are no-ops.
func (e *Engine) applyDelete(nodeID string) error {
	entry, ok, err := e.state.GetByNodeID(nodeID)
	if err != nil || !ok {
		return err // never synced, or already gone: nothing to do
	}

	if entry.Kind == "folder" {
		if err := os.RemoveAll(e.localPath(entry.Path)); err != nil {
			return err
		}
		return e.forgetSubtree(entry.Path)
	}

	exists, changed, err := e.localStatus(entry)
	if err != nil {
		return err
	}
	// A delete that would destroy an unseen local edit is the same data loss as an
	// overwrite. The edited file is set aside under a conflict name — resurfacing
	// as a copy the push uploads as a new file — rather than being honoured into
	// oblivion. conflictCopy drops the original path's state itself.
	if exists && changed {
		_, err := e.conflictCopy(entry)
		return err
	}
	if err := os.Remove(e.localPath(entry.Path)); err != nil && !os.IsNotExist(err) {
		return err
	}
	e.log.Info("removed", "path", entry.Path)
	return e.state.Delete(entry.Path)
}

// removeVanished handles a reconcile finding a recorded node absent from the
// server: the same rule as a delete, applied to whole entries.
func (e *Engine) removeVanished(entry state.Entry) error {
	if entry.Kind == "folder" {
		// A folder is only removed once its recorded children are; a full reconcile
		// visits every entry, so leave the directory removal to os.RemoveAll here
		// and forget the record.
		if err := os.RemoveAll(e.localPath(entry.Path)); err != nil {
			return err
		}
		return e.forgetSubtree(entry.Path)
	}
	exists, changed, err := e.localStatus(entry)
	if err != nil {
		return err
	}
	if exists && changed {
		// Vanished on the server but edited here: preserve the edit as a conflict
		// copy rather than deleting it, same as a delete seen through the journal.
		_, err := e.conflictCopy(entry)
		return err
	}
	if err := os.Remove(e.localPath(entry.Path)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return e.state.Delete(entry.Path)
}

// moveLocal relocates a file or folder on disk and updates its state key.
func (e *Engine) moveLocal(prev state.Entry, newPath string) error {
	dst := e.localPath(newPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(e.localPath(prev.Path), dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := e.state.Delete(prev.Path); err != nil {
		return err
	}
	prev.Path = newPath
	return e.state.Put(prev)
}

// forgetSubtree drops every state record at or beneath a path.
func (e *Engine) forgetSubtree(root string) error {
	entries, err := e.state.List()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if isUnder(entry.Path, root) {
			if err := e.state.Delete(entry.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

// localStatus reports whether a recorded file exists on disk and whether its
// content differs from what was synced. mtime and size are the cheap gate; only
// when they move is the file actually re-hashed, so an untouched tree costs a
// stat per file, not a read.
func (e *Engine) localStatus(entry state.Entry) (exists, changed bool, err error) {
	info, err := os.Stat(e.localPath(entry.Path))
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if info.IsDir() {
		return true, false, nil
	}
	if info.Size() == entry.Size && info.ModTime().Unix() == entry.MtimeUnix {
		return true, false, nil
	}
	h, err := hashFile(e.localPath(entry.Path))
	if err != nil {
		return true, false, err
	}
	return true, h != entry.Hash, nil
}

// hashFile computes a file's whole-file BLAKE3, the client's local content
// identity — independent of how the server happens to address it.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := blake3.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return hex.EncodeToString(sum[:]), nil
}
