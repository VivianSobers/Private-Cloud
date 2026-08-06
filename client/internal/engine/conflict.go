package engine

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/state"
)

// A conflict is detected by lineage, never by clocks: a file was synced at some
// base version, and since then BOTH the server's head moved past it and the local
// file changed. The pull spots this because the server's content hash no longer
// equals the base it recorded (RemoteHash) while the local bytes no longer equal
// the base it recorded (Hash). Timestamps never enter into it, so clock skew
// between two machines cannot fabricate or mask a conflict.
//
// The resolution never overwrites and never merges: the local edit is set aside
// under a conflict name and the original name is left for the server's version.
// The user resolves by choosing between two files — a decision a person can make
// and an automatic merge cannot.

// conflictCopy sets the local edit aside under a conflict name and drops the
// original path's state, so the freed name can take the server's version and the
// set-aside file is pushed as a new file of its own. It returns the server path
// the copy now occupies.
func (e *Engine) conflictCopy(entry state.Entry) (string, error) {
	src := e.localPath(entry.Path)

	// Find a conflict name whose local path is free, so a second conflict on the
	// same day does not clobber the first before it has been pushed.
	var dstServer string
	for n := 1; ; n++ {
		candidate := e.conflictServerPath(entry.Path, n)
		if _, err := os.Stat(e.localPath(candidate)); os.IsNotExist(err) {
			dstServer = candidate
			break
		} else if err != nil {
			return "", err
		}
	}

	dst := e.localPath(dstServer)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	// If the local file is already gone the conflict is moot, but the state must
	// still be dropped so the name is free.
	if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := e.state.Delete(entry.Path); err != nil {
		return "", err
	}
	e.log.Warn("conflict copy created", "original", entry.Path, "copy", dstServer)
	return dstServer, nil
}

// conflictServerPath builds the name a conflict copy takes: the original stem, a
// parenthetical naming the machine and date, then the original extension — so the
// copy sorts next to its sibling and keeps its type. n>1 disambiguates repeats.
func (e *Engine) conflictServerPath(serverPath string, n int) string {
	dir := parentPath(serverPath)
	base := baseName(serverPath)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	suffix := fmt.Sprintf(" (conflict from %s %s)", e.hostname, e.clock().Format("2006-01-02"))
	if n > 1 {
		suffix += fmt.Sprintf(" (%d)", n)
	}
	name := stem + suffix + ext
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}
