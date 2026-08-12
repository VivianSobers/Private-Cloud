package engine

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/ignore"
)

// ignoreFileName is the per-root ignore file, read fresh at the start of each
// reconcile so edits take effect on the next pass without a restart.
const ignoreFileName = ".pcsyncignore"

// loadIgnore reads <root>/.pcsyncignore and compiles it. A missing or unreadable
// file is the norm, not an error — it just means "ignore nothing".
func (e *Engine) loadIgnore() {
	data, err := os.ReadFile(filepath.Join(e.root, ignoreFileName))
	if err != nil {
		e.ignore.Store(ignore.Compile(nil))
		return
	}
	e.ignore.Store(ignore.Compile(strings.Split(string(data), "\n")))
}

// ignored reports whether a server path matches the active ignore rules.
func (e *Engine) ignored(serverPath string, isDir bool) bool {
	m := e.ignore.Load()
	return m != nil && m.Match(serverPath, isDir)
}
