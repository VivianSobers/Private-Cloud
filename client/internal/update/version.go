// Package update is pcsync's in-place updater: it reads a signed release feed,
// verifies a new build against both its SHA-256 and the Sigstore signature over
// the checksum file, and swaps the running binary for it atomically.
//
// The shape of the trust here is worth stating plainly, because an updater is
// the one part of a sync client that can turn a bad answer into arbitrary code
// on the machine:
//
//   - The feed is a hint, nothing more. It says which version exists and where
//     the files are. Nothing in it is trusted: a rewritten feed can make pcsync
//     download the wrong bytes, and every check below still has to pass.
//   - SHA256SUMS is the manifest of record, and the cosign signature is over
//     that file. One signature therefore covers every artifact of a release,
//     and an attacker who can swap a binary still has to forge the sums file.
//   - The signature is keyless — a short-lived Fulcio certificate bound to the
//     GitHub Actions workflow that built the release. The updater pins both the
//     certificate identity (which repository, which workflow, which tag ref) and
//     the OIDC issuer, so "signed by somebody with a GitHub account" is not the
//     whole of the check.
//   - What is NOT checked in-process: Rekor transparency-log inclusion. That
//     needs the log's public key and a network round trip to the log, and doing
//     it badly is worse than not claiming it. `cosign verify-blob` on a desktop
//     does the full check, and docs/install.md tells people how. Said plainly:
//     this verifies the signature and its certificate chain, not the log.
package update

import (
	"strconv"
	"strings"
)

// Version is a parsed pcsync release version: the three numeric components plus
// an optional pre-release tag. Release tags look like v1.2.3 or v1.2.3-rc.1;
// development builds are git-describe strings like v1.2.3-4-gabc1234-dirty, and
// the bare string "dev" for an unstamped build.
type Version struct {
	Major, Minor, Patch int
	Pre                 string // "" for a final release
}

// ParseVersion reads a release version. It returns ok=false for anything it
// cannot compare honestly — "dev", a git-describe string with a commit count, an
// empty string — because the alternative is to guess an ordering and then act on
// the guess by overwriting somebody's binary.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return Version{}, false
	}
	// Build metadata (+...) does not participate in ordering; drop it.
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var v Version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return Version{}, false
		}
		switch i {
		case 0:
			v.Major = n
		case 1:
			v.Minor = n
		case 2:
			v.Patch = n
		}
	}
	// A git-describe suffix (-4-gabc1234[-dirty]) means "somewhere after 1.2.3",
	// which is not a point on the line, so it is not comparable.
	if isDescribeSuffix(pre) {
		return Version{}, false
	}
	v.Pre = pre
	return v, true
}

// isDescribeSuffix reports whether a pre-release field is really a `git describe`
// tail: <commits>-g<hash>, optionally followed by -dirty.
func isDescribeSuffix(pre string) bool {
	if pre == "" {
		return false
	}
	fields := strings.Split(pre, "-")
	if len(fields) < 2 {
		return false
	}
	if _, err := strconv.Atoi(fields[0]); err != nil {
		return false
	}
	return strings.HasPrefix(fields[1], "g")
}

// Compare orders two versions: -1 if a sorts before b, 0 if equal, +1 after.
// Pre-release ordering follows semver's rule — 1.2.0-rc.1 precedes 1.2.0 — and
// two pre-release tags are compared as plain strings, which is enough for the
// rc.N and beta.N tags this project uses and is honest about being no more.
func Compare(a, b Version) int {
	switch {
	case a.Major != b.Major:
		return sign(a.Major - b.Major)
	case a.Minor != b.Minor:
		return sign(a.Minor - b.Minor)
	case a.Patch != b.Patch:
		return sign(a.Patch - b.Patch)
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1 // a final release outranks any pre-release of the same numbers
	case b.Pre == "":
		return -1
	case a.Pre < b.Pre:
		return -1
	default:
		return 1
	}
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	return 1
}

// Newer reports whether candidate is a strictly later version than current. An
// unparseable current version (a dev build) is treated as "not comparable", and
// the caller decides what that means — the daemon declines to auto-update a dev
// build, because replacing a binary somebody built themselves with a release is
// not an update, it is a surprise.
func Newer(current, candidate string) (newer, comparable bool) {
	cur, okCur := ParseVersion(current)
	cand, okCand := ParseVersion(candidate)
	if !okCur || !okCand {
		return false, false
	}
	return Compare(cand, cur) > 0, true
}
