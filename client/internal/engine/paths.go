package engine

import (
	"path"
	"path/filepath"
	"strings"
)

// The engine maps between two path spaces: server paths, which are slash-separated
// and rooted at "/", and local filesystem paths under the synced root. Keeping the
// conversion in one place is what stops a Windows backslash or a stray trailing
// slash from turning into a second, divergent copy of a file.

// localPath resolves a server path to its location under the synced root.
func (e *Engine) localPath(serverPath string) string {
	rel := strings.TrimPrefix(serverPath, "/")
	if rel == "" {
		return e.root
	}
	return filepath.Join(e.root, filepath.FromSlash(rel))
}

// serverPath resolves a local absolute path back to its server path. The root
// itself is "/".
func (e *Engine) serverPath(localAbs string) (string, bool) {
	rel, err := filepath.Rel(e.root, localAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "/", true
	}
	return "/" + filepath.ToSlash(rel), true
}

// parentPath returns the server path of a node's parent. The parent of a
// top-level node is the root "/".
func parentPath(serverPath string) string {
	return path.Dir(serverPath)
}

// baseName returns the final segment of a server path.
func baseName(serverPath string) string {
	return path.Base(serverPath)
}

// isUnder reports whether child is the same as or nested beneath parent, used to
// sweep a folder's descendants out of local state when the folder is removed.
func isUnder(child, parent string) bool {
	if parent == "/" {
		return true
	}
	return child == parent || strings.HasPrefix(child, parent+"/")
}
