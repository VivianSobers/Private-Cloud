package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Feed is the release feed: a small JSON document published as a release asset
// that says which version exists and where its files are.
//
// Nothing in it is trusted. It is fetched over TLS, but a feed that has been
// rewritten — by a compromised host, a proxy, a mirror — can only ever redirect
// the updater to different bytes, and those bytes still have to appear in a
// SHA256SUMS carrying a valid signature from this project's release workflow.
// The feed decides *what to look at*; the signature decides *what to install*.
type Feed struct {
	// Version is the release tag, e.g. "v1.3.0".
	Version string `json:"version"`
	// ReleasedAt is informational, shown to the user.
	ReleasedAt time.Time `json:"released_at"`
	// NotesURL points at the release notes.
	NotesURL string `json:"notes_url"`
	// BaseURL is the directory the artifacts live under; an artifact filename is
	// appended to it.
	BaseURL string `json:"base_url"`
	// ChecksumsURL, SignatureURL and CertificateURL are the SHA256SUMS file and
	// the cosign keyless signature and certificate over it. One signature covers
	// the whole release, because the file it signs names every artifact.
	ChecksumsURL   string `json:"checksums_url"`
	SignatureURL   string `json:"signature_url"`
	CertificateURL string `json:"certificate_url"`
}

// Release is the answer to "is there anything to install?" — the feed plus the
// comparison against the running build.
type Release struct {
	Feed Feed
	// Newer is true when the feed's version sorts strictly after this build's.
	Newer bool
	// Comparable is false when either version is not a release tag (a dev build,
	// a git-describe string). An incomparable pair is never auto-applied.
	Comparable bool
	// Current is the running build's version, echoed for reporting.
	Current string
}

// Result describes a completed update.
type Result struct {
	From, To string
	Path     string
}

// Options configures an Updater. The package deliberately takes plain values
// rather than importing the config package: the updater is the one component
// that must be testable without a config file on disk.
type Options struct {
	// CurrentVersion is the running build's version string.
	CurrentVersion string
	// FeedURL is the release feed.
	FeedURL string
	// Verifier carries the trust anchors and the accepted signing identity.
	Verifier *Verifier
	// AllowDowngrade permits installing an older version. Off by default.
	AllowDowngrade bool
	// TargetPath is the binary to replace; empty means the running executable.
	TargetPath string
	// GOOS and GOARCH pick the artifact; empty means this build's own.
	GOOS, GOARCH string
	// HTTPClient is used for every fetch; nil means a client with a sane timeout.
	HTTPClient *http.Client
}

// maxArtifact bounds what the updater will read from the network. pcsync is a
// ten-megabyte static binary; anything an order of magnitude past that is either
// a mistake or somebody trying to fill a laptop's disk from a JSON file.
const (
	maxArtifact = 128 << 20
	maxMetadata = 4 << 20
)

// Updater checks a release feed and installs what it finds, after verifying it.
type Updater struct {
	opts Options
	http *http.Client
}

// New builds an Updater. It fails rather than defaulting when there is nothing
// to verify against — an updater without a Verifier is a downloader, and this
// package should not be able to become one by omission.
func New(opts Options) (*Updater, error) {
	if opts.Verifier == nil {
		return nil, errors.New("update: refusing to run without a signature verifier")
	}
	if !strings.HasPrefix(opts.FeedURL, "https://") {
		return nil, errors.New("update: feed URL must be https")
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	c := opts.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Updater{opts: opts, http: c}, nil
}

// ArtifactName is the release filename for a target, matching what
// build-release.sh writes into dist/. The two have to agree; they agree by
// spelling the rule once here and once there, and by the release workflow
// running the updater's own check against the artifacts it just built.
func ArtifactName(goos, goarch string) string {
	name := fmt.Sprintf("pcsync-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Check fetches the feed and reports what it offers. It performs no writes and
// no verification — verification belongs to Apply, where it gates an install
// rather than a message.
func (u *Updater) Check(ctx context.Context) (*Release, error) {
	body, err := u.fetch(ctx, u.opts.FeedURL, maxMetadata)
	if err != nil {
		return nil, fmt.Errorf("update: fetch feed: %w", err)
	}
	var f Feed
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("update: parse feed: %w", err)
	}
	if strings.TrimSpace(f.Version) == "" {
		return nil, errors.New("update: feed names no version")
	}
	newer, comparable := Newer(u.opts.CurrentVersion, f.Version)
	return &Release{Feed: f, Newer: newer, Comparable: comparable, Current: u.opts.CurrentVersion}, nil
}

// Apply downloads the release, verifies it, and swaps it in.
//
// The order is the whole point, so it is spelled out: fetch SHA256SUMS, verify
// the cosign signature over that file, only then look up this artifact's hash in
// it, download the artifact, hash it while writing, and refuse if the digest
// differs. Nothing that failed a check is ever moved into place — the download
// lands in a temp file next to the target and is either renamed over it or
// deleted.
func (u *Updater) Apply(ctx context.Context, rel *Release) (Result, error) {
	var zero Result
	if rel == nil {
		return zero, errors.New("update: nothing to apply")
	}
	if !u.opts.AllowDowngrade {
		if !rel.Comparable {
			return zero, fmt.Errorf("update: cannot compare %q with %q; run with allow_downgrade to install anyway",
				u.opts.CurrentVersion, rel.Feed.Version)
		}
		if !rel.Newer {
			// A feed rolled back to an old, signed, known-bad release is a real
			// attack on a signed-artifact channel: every signature still checks
			// out. Version order is the defence, so it is enforced here and not
			// only in Check.
			return zero, fmt.Errorf("update: %s is not newer than the running %s; refusing to downgrade",
				rel.Feed.Version, u.opts.CurrentVersion)
		}
	}

	sums, err := u.fetch(ctx, rel.Feed.ChecksumsURL, maxMetadata)
	if err != nil {
		return zero, fmt.Errorf("update: fetch checksums: %w", err)
	}
	sig, err := u.fetch(ctx, rel.Feed.SignatureURL, maxMetadata)
	if err != nil {
		return zero, fmt.Errorf("update: fetch signature: %w", err)
	}
	cert, err := u.fetch(ctx, rel.Feed.CertificateURL, maxMetadata)
	if err != nil {
		return zero, fmt.Errorf("update: fetch certificate: %w", err)
	}
	if err := u.opts.Verifier.Verify(sums, cert, string(sig)); err != nil {
		return zero, err
	}

	name := ArtifactName(u.opts.GOOS, u.opts.GOARCH)
	want, err := digestFor(sums, name)
	if err != nil {
		return zero, err
	}

	target := u.opts.TargetPath
	if target == "" {
		if target, err = os.Executable(); err != nil {
			return zero, fmt.Errorf("update: locate running binary: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(target); err == nil {
			target = resolved
		}
	}

	artifactURL, err := joinURL(rel.Feed.BaseURL, name)
	if err != nil {
		return zero, err
	}
	staged, err := u.download(ctx, artifactURL, target, want)
	if err != nil {
		return zero, err
	}
	defer os.Remove(staged) // a no-op once the rename below has consumed it

	if err := replaceBinary(target, staged); err != nil {
		return zero, err
	}
	return Result{From: u.opts.CurrentVersion, To: rel.Feed.Version, Path: target}, nil
}

// download streams the artifact into a temp file beside the target and verifies
// its digest before returning. Writing beside the target rather than into the
// system temp directory is deliberate: the final step has to be a rename, and a
// rename is only atomic within one filesystem.
func (u *Updater) download(ctx context.Context, src, target, wantHex string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("update: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update: download %s: HTTP %d", src, resp.StatusCode)
	}

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".pcsync-update-*")
	if err != nil {
		return "", fmt.Errorf("update: stage download in %s: %w", dir, err)
	}
	name := tmp.Name()
	sum := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(resp.Body, maxArtifact))
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(name)
		return "", fmt.Errorf("update: write download: %w", errors.Join(copyErr, closeErr))
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != wantHex {
		os.Remove(name)
		return "", fmt.Errorf("update: downloaded %s has digest %s, expected %s from the signed checksums",
			path.Base(src), got, wantHex)
	}
	// The staged file is a program about to become *the* program; give it the
	// mode before it is renamed, so no moment exists where the target is in place
	// but not executable.
	if err := os.Chmod(name, 0o755); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("update: mark download executable: %w", err)
	}
	return name, nil
}

// digestFor finds one artifact's digest in a SHA256SUMS file. It is strict about
// the format because it is parsing the document that decides what gets executed:
// a line it cannot read is an error, never a skip.
func digestFor(sums []byte, artifact string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		digest, file, ok := strings.Cut(line, " ")
		if !ok {
			return "", fmt.Errorf("update: malformed line in checksums: %q", line)
		}
		// sha256sum writes "<hex>  <name>" (two spaces) or "<hex> *<name>" for a
		// binary read; both leave the name after the separator run.
		file = strings.TrimSpace(file)
		file = strings.TrimPrefix(file, "*")
		file = strings.TrimPrefix(file, "./")
		if file != artifact {
			continue
		}
		if len(digest) != sha256.Size*2 {
			return "", fmt.Errorf("update: %s has a malformed digest in the checksums", artifact)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return "", fmt.Errorf("update: %s has a non-hex digest in the checksums", artifact)
		}
		return strings.ToLower(digest), nil
	}
	return "", fmt.Errorf("update: this release publishes no %s", artifact)
}

// joinURL appends an artifact filename to the feed's base URL, refusing anything
// that is not a plain name — the feed must not be able to walk the updater out
// of the release directory or onto another host.
func joinURL(base, name string) (string, error) {
	if !strings.HasPrefix(base, "https://") {
		return "", fmt.Errorf("update: feed base URL %q is not https", base)
	}
	if name != path.Base(name) || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("update: refusing artifact name %q", name)
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("update: feed base URL: %w", err)
	}
	u.Path = path.Join(u.Path, name)
	return u.String(), nil
}

// fetch reads a metadata document, bounded.
func (u *Updater) fetch(ctx context.Context, src string, limit int64) ([]byte, error) {
	if !strings.HasPrefix(src, "https://") {
		return nil, fmt.Errorf("%q is not an https URL", src)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", src, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
