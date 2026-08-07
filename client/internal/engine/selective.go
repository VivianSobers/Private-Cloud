package engine

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// excludesMetaKey is where the selective-sync set is persisted in the state db,
// so exclusions survive a restart rather than resetting to the config's seed.
const excludesMetaKey = "excludes"

// Selective sync lets a device decline whole subtrees of the server tree — the
// classic "don't put my 200 GB Videos folder on this laptop". It is expressed as
// a set of excluded server-path prefixes; a node is excluded if it is at or
// beneath any of them. Excluded subtrees are never downloaded, never uploaded,
// and — crucially — their absence locally never deletes them on the server: the
// server stays the complete copy, and each device chooses its own subset of it.
//
// The exclude set is held in an atomic pointer so the control goroutine can
// change it live (a tray toggling a folder) while the sync loop reads it per
// node, with no lock on the hot path.

// excludes is *[]string of normalized prefixes; nil means "exclude nothing".
func (e *Engine) loadExcludes() []string {
	if p := e.excludes.Load(); p != nil {
		return *p
	}
	return nil
}

// excluded reports whether a server path falls inside any excluded subtree.
func (e *Engine) excluded(serverPath string) bool {
	for _, prefix := range e.loadExcludes() {
		if isUnder(serverPath, prefix) {
			return true
		}
	}
	return false
}

// Excludes returns a copy of the current exclude set, for a status/GET view.
func (e *Engine) Excludes() []string {
	return append([]string(nil), e.loadExcludes()...)
}

// SetExcludes replaces the exclude set, normalizing the input, and persists it so
// it survives a restart. The next reconcile prunes anything newly excluded and
// stops touching it thereafter.
func (e *Engine) SetExcludes(prefixes []string) {
	norm := normalizeExcludes(prefixes)
	e.excludes.Store(&norm)
	// Persist best-effort: a failed write leaves the running set correct and only
	// risks reverting to the seed on the next restart, which the log flags.
	if data, err := json.Marshal(norm); err == nil {
		if err := e.state.SetMeta(excludesMetaKey, string(data)); err != nil {
			e.log.Warn("could not persist excludes", "error", err)
		}
	}
}

// SeedExcludes applies the config's exclude set, but only when none has ever been
// persisted — so a live change made through the control surface is authoritative
// and is not clobbered by the config's seed on the next start. Clearing every
// exclusion (a persisted empty set) is remembered too, not re-seeded.
func (e *Engine) SeedExcludes(fromConfig []string) {
	if e.excludesLoaded {
		return
	}
	e.SetExcludes(fromConfig)
}

// loadPersistedExcludes reads the saved exclude set at startup. Best-effort: a
// missing or unreadable value just leaves the set empty for the config to seed.
func (e *Engine) loadPersistedExcludes() {
	raw, ok, err := e.state.Meta(excludesMetaKey)
	if err != nil || !ok {
		return
	}
	e.excludesLoaded = true
	var xs []string
	if json.Unmarshal([]byte(raw), &xs) != nil {
		return
	}
	norm := normalizeExcludes(xs)
	e.excludes.Store(&norm)
}

// normalizeExcludes cleans raw prefixes into absolute, slash-rooted paths with no
// trailing slash, dropping blanks and the root itself — excluding "/" would mean
// "sync nothing", which a config typo must not silently do. The result is sorted
// and de-duplicated.
func normalizeExcludes(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		p = strings.TrimRight(p, "/")
		if p == "" || seen[p] { // "/" collapses to "" here — skip it
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// pruneExcluded drops local tracking of any subtree that is now excluded, and
// reclaims its disk space when it is safe to do so. It is the transition handler
// for "this folder was syncing, now it should not".
//
// Safety rule: a subtree with no local modifications is identical to the server's
// copy, so removing it locally loses nothing and frees the space. A subtree with
// any local edit is left entirely on disk — those files simply stop being synced,
// becoming ordinary local files — because deleting an unsynced edit would be data
// loss. Either way the server is never touched: exclusion is a local decision.
func (e *Engine) pruneExcluded() error {
	prefixes := e.loadExcludes()
	if len(prefixes) == 0 {
		return nil
	}
	entries, err := e.state.List()
	if err != nil {
		return err
	}

	// Which excluded prefixes have a locally-modified file under them: those are
	// kept on disk. A prefix with none is safe to remove wholesale.
	dirty := map[string]bool{}
	tracked := map[string]bool{} // prefixes that actually have tracked entries
	for _, entry := range entries {
		prefix, ok := matchingPrefix(entry.Path, prefixes)
		if !ok {
			continue
		}
		tracked[prefix] = true
		if entry.Kind != "file" || dirty[prefix] {
			continue
		}
		if exists, changed, err := e.localStatus(entry); err != nil {
			return err
		} else if exists && changed {
			dirty[prefix] = true
		}
	}

	for prefix := range tracked {
		if dirty[prefix] {
			e.log.Warn("excluded subtree kept on disk (has local edits)", "path", prefix)
		} else {
			if err := os.RemoveAll(e.localPath(prefix)); err != nil {
				return err
			}
			e.log.Info("excluded subtree pruned locally", "path", prefix)
		}
		// Stop tracking the whole subtree regardless — it is no longer synced.
		if err := e.forgetSubtree(prefix); err != nil {
			return err
		}
	}
	return nil
}

// matchingPrefix returns the first excluded prefix a path falls under.
func matchingPrefix(serverPath string, prefixes []string) (string, bool) {
	for _, prefix := range prefixes {
		if isUnder(serverPath, prefix) {
			return prefix, true
		}
	}
	return "", false
}
