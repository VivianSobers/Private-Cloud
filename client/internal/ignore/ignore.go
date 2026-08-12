// Package ignore matches paths against a .pcsyncignore file — a pragmatic subset
// of gitignore semantics, so a device never syncs local junk like build output,
// editor scratch files or OS metadata.
//
// It is the complement of selective sync: selective sync declines whole *server*
// subtrees a device does not want, while ignore declines *local* paths by pattern
// regardless of where they sit. Both keep the server authoritative and neither
// deletes anything on it.
//
// Supported syntax (a deliberate subset — a rule people cannot predict is one
// they misconfigure):
//
//   - Blank lines and lines starting with '#' are ignored.
//   - A trailing '/' means the pattern matches directories only ("node_modules/").
//     The walk then skips the whole subtree, so its contents need no rule of their
//     own.
//   - A pattern containing '/' is anchored to the sync root and matched against
//     the whole path ("build", "/build", "src/*.log"). A leading '/' is optional
//     and stripped.
//   - A pattern with no '/' is matched against each path's final segment, so it
//     applies at any depth ("*.tmp", ".DS_Store").
//   - '*', '?' and '[…]' are glob wildcards (via path.Match); '**' is not special.
package ignore

import (
	"path"
	"strings"
)

type pattern struct {
	glob     string // the glob to match (leading/trailing slashes stripped)
	anchored bool   // pattern contained '/', so match the whole path not the basename
	dirOnly  bool   // pattern ended in '/', so it matches directories only
}

// Matcher is a compiled set of ignore rules.
type Matcher struct {
	patterns []pattern
}

// Compile builds a matcher from raw lines (the contents of a .pcsyncignore file,
// split by newline). Unparseable-looking lines are simply kept as literal globs;
// there is no error, because an ignore file must never fail a sync.
func Compile(lines []string) *Matcher {
	var ps []pattern
	for _, ln := range lines {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		dirOnly := strings.HasSuffix(s, "/")
		s = strings.TrimSuffix(s, "/")
		if s == "" {
			continue
		}
		anchored := strings.Contains(s, "/")
		ps = append(ps, pattern{
			glob:     strings.TrimPrefix(s, "/"),
			anchored: anchored,
			dirOnly:  dirOnly,
		})
	}
	return &Matcher{patterns: ps}
}

// Empty reports whether there are no rules — the fast path for the common case of
// no ignore file at all.
func (m *Matcher) Empty() bool { return m == nil || len(m.patterns) == 0 }

// Count returns how many rules are active.
func (m *Matcher) Count() int {
	if m == nil {
		return 0
	}
	return len(m.patterns)
}

// Match reports whether a server-relative path (always beginning with '/') is
// ignored. isDir must say whether the path is a directory, so a directory-only
// rule does not swallow a like-named file.
func (m *Matcher) Match(serverPath string, isDir bool) bool {
	if m.Empty() {
		return false
	}
	rel := strings.TrimPrefix(serverPath, "/")
	if rel == "" {
		return false // never ignore the root itself
	}
	base := path.Base(rel)
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		target := base
		if p.anchored {
			target = rel
		}
		if ok, _ := path.Match(p.glob, target); ok {
			return true
		}
	}
	return false
}
