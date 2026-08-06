package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceDelay coalesces a burst of filesystem events — a directory being
// unpacked fires hundreds — into a single push once the writes settle.
const debounceDelay = time.Second

// Run drives the daemon until the context is cancelled.
//
// Three cadences, exactly the design's two loops plus their safety net:
//
//   - poll:   apply remote changes from the journal, frequently and cheaply.
//   - events: fsnotify signals a local change; a debounced push follows.
//   - rescan: a full pull+push on a slower timer, so a missed inotify event is
//     caught by a real comparison rather than lost.
//
// The select serializes them: a pull, a push and a rescan never overlap, so no
// lock is needed around the state database or the tree.
func (e *Engine) Run(ctx context.Context, poll, rescan time.Duration) error {
	if err := e.Sync(ctx); err != nil {
		e.log.Error("initial sync failed", "error", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	e.addWatches(watcher)

	pollTicker := time.NewTicker(poll)
	defer pollTicker.Stop()
	rescanTicker := time.NewTicker(rescan)
	defer rescanTicker.Stop()

	// A stopped timer that later firings Reset; the initial long delay never fires.
	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-pollTicker.C:
			if err := e.pullSafe(ctx); err != nil {
				e.log.Error("pull failed", "error", err)
			}

		case <-rescanTicker.C:
			if err := e.Sync(ctx); err != nil {
				e.log.Error("rescan failed", "error", err)
			}
			e.addWatches(watcher)

		case ev := <-watcher.Events:
			if strings.HasPrefix(ev.Name, e.stateDir) {
				continue // our own database and temp files are not sync events
			}
			// Watch a newly-created directory so its contents are seen too.
			if ev.Op&fsnotify.Create != 0 {
				if info, serr := os.Stat(ev.Name); serr == nil && info.IsDir() {
					_ = watcher.Add(ev.Name)
				}
			}
			debounce.Reset(debounceDelay)

		case <-debounce.C:
			if err := e.pushSafe(ctx); err != nil {
				e.log.Error("push failed", "error", err)
			}
			e.addWatches(watcher)

		case werr := <-watcher.Errors:
			e.log.Warn("watcher error", "error", werr)
		}
	}
}

// pullSafe applies remote changes, refreshing the cached root first.
func (e *Engine) pullSafe(ctx context.Context) error {
	if err := e.ensureRoot(ctx); err != nil {
		return err
	}
	return e.pull(ctx)
}

// pushSafe pushes local changes, refreshing the cached root first.
func (e *Engine) pushSafe(ctx context.Context) error {
	if err := e.ensureRoot(ctx); err != nil {
		return err
	}
	return e.push(ctx)
}

// addWatches registers every directory under the root except the state directory.
// fsnotify does not recurse, so each directory is added explicitly; adding one
// already watched is harmless.
func (e *Engine) addWatches(watcher *fsnotify.Watcher) {
	_ = filepath.WalkDir(e.root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a directory that vanished mid-walk is not worth aborting for
		}
		if abs == e.stateDir {
			return filepath.SkipDir
		}
		if d.IsDir() {
			_ = watcher.Add(abs)
		}
		return nil
	})
}
