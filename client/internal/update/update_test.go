package update

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// --- a stand-in Sigstore ------------------------------------------------------
//
// These tests mint their own certificate authority rather than reaching for the
// embedded Fulcio roots. The point is to exercise the *rules* — chains to the
// pinned CA, carries the expected identity, signs the exact bytes — and a test
// that needed the real Sigstore would exercise the network instead.

const (
	testIdentity = "https://github.com/guru-bharadwaj20/private-cloud/.github/workflows/release.yml@refs/tags/v1.3.0"
	testIssuer   = "https://token.actions.githubusercontent.com"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"test.invalid"}, CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

type leafOpts struct {
	identity  string
	issuer    string // "" omits the extension entirely
	notBefore time.Time
}

// issue mints a short-lived signing certificate shaped like Fulcio's: a URI SAN
// naming the workflow, the OIDC issuer in extension 1.3.6.1.4.1.57264.1.8, and a
// ten-minute lifetime.
func (ca *testCA) issue(t *testing.T, o leafOpts) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nb := o.notBefore
	if nb.IsZero() {
		nb = time.Now().Add(-30 * time.Minute)
	}
	san, err := url.Parse(o.identity)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		NotBefore:    nb,
		NotAfter:     nb.Add(10 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{san},
	}
	if o.issuer != "" {
		value, err := marshalIssuer(o.issuer)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.ExtraExtensions = []pkix.Extension{{Id: oidIssuerV2, Value: value}}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key
}

// marshalIssuer DER-encodes the issuer the way Fulcio's v2 extension does.
func marshalIssuer(s string) ([]byte, error) {
	return asn1.Marshal(s)
}

func signBlob(t *testing.T, key *ecdsa.PrivateKey, payload []byte) string {
	t.Helper()
	sum := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func trust(ca *testCA, identity string) *Verifier {
	return &Verifier{
		Roots:    ca.pool,
		Identity: regexp.MustCompile(identity),
		Issuer:   testIssuer,
	}
}

// --- signature verification ---------------------------------------------------

func TestVerifyAcceptsAReleaseSignedByTheReleaseWorkflow(t *testing.T) {
	ca := newTestCA(t)
	certPEM, key := ca.issue(t, leafOpts{identity: testIdentity, issuer: testIssuer})
	payload := []byte("deadbeef  pcsync-linux-amd64\n")

	v := trust(ca, `^https://github\.com/guru-bharadwaj20/private-cloud/\.github/workflows/release\.yml@refs/tags/`)
	if err := v.Verify(payload, certPEM, signBlob(t, key, payload)); err != nil {
		t.Fatalf("a good signature was rejected: %v", err)
	}
}

func TestVerifyRejectsEveryWayItShould(t *testing.T) {
	ca := newTestCA(t)
	other := newTestCA(t)
	payload := []byte("deadbeef  pcsync-linux-amd64\n")
	goodCert, goodKey := ca.issue(t, leafOpts{identity: testIdentity, issuer: testIssuer})

	otherCert, otherKey := other.issue(t, leafOpts{identity: testIdentity, issuer: testIssuer})
	wrongIDCert, wrongIDKey := ca.issue(t, leafOpts{
		identity: "https://github.com/someone-else/private-cloud/.github/workflows/release.yml@refs/tags/v1.3.0",
		issuer:   testIssuer,
	})
	wrongIssuerCert, wrongIssuerKey := ca.issue(t, leafOpts{identity: testIdentity, issuer: "https://accounts.google.com"})
	noIssuerCert, noIssuerKey := ca.issue(t, leafOpts{identity: testIdentity})
	futureCert, futureKey := ca.issue(t, leafOpts{
		identity: testIdentity, issuer: testIssuer, notBefore: time.Now().Add(90 * 24 * time.Hour),
	})

	pattern := `^https://github\.com/guru-bharadwaj20/private-cloud/\.github/workflows/release\.yml@refs/tags/`
	cases := []struct {
		name    string
		payload []byte
		cert    []byte
		sig     string
		want    string
	}{
		{
			// The payload changed after signing: a tampered SHA256SUMS, which is
			// the whole attack the signature exists to stop.
			name: "tampered checksums", payload: []byte("cafebabe  pcsync-linux-amd64\n"),
			cert: goodCert, sig: signBlob(t, goodKey, payload),
			want: "does not match",
		},
		{
			name: "signature from a certificate we were not given", payload: payload,
			cert: goodCert, sig: signBlob(t, otherKey, payload),
			want: "does not match",
		},
		{
			name: "certificate from an untrusted CA", payload: payload,
			cert: otherCert, sig: signBlob(t, otherKey, payload),
			want: "does not chain",
		},
		{
			name: "somebody else's repository", payload: payload,
			cert: wrongIDCert, sig: signBlob(t, wrongIDKey, payload),
			want: "not one this build accepts",
		},
		{
			name: "right identity string, wrong issuer", payload: payload,
			cert: wrongIssuerCert, sig: signBlob(t, wrongIssuerKey, payload),
			want: "was issued by",
		},
		{
			name: "no issuer at all", payload: payload,
			cert: noIssuerCert, sig: signBlob(t, noIssuerKey, payload),
			want: "names no OIDC issuer",
		},
		{
			// The chain is checked at the certificate's own NotBefore, so a
			// certificate dated in the future would otherwise verify against a
			// time of its own choosing.
			name: "certificate dated in the future", payload: payload,
			cert: futureCert, sig: signBlob(t, futureKey, payload),
			want: "in the future",
		},
		{
			name: "signature is not base64", payload: payload,
			cert: goodCert, sig: "not base64 at all!!",
			want: "not base64",
		},
		{
			name: "no certificate", payload: payload,
			cert: []byte("just some prose"), sig: signBlob(t, goodKey, payload),
			want: "certificate is empty",
		},
	}

	v := trust(ca, pattern)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := v.Verify(c.payload, c.cert, c.sig)
			if err == nil {
				t.Fatal("verification passed; it must not")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestFulcioTrustLoadsThePinnedRoots(t *testing.T) {
	// Not a signature test — a "the embedded PEM is still a readable CA" test, so
	// a bad refresh of fulcio_roots.pem fails here rather than on a user's laptop.
	v, err := FulcioTrust(`^https://github\.com/`, testIssuer)
	if err != nil {
		t.Fatalf("embedded Fulcio roots did not load: %v", err)
	}
	if v.Roots == nil || len(v.Roots.Subjects()) == 0 { //nolint:staticcheck // Subjects is fine for a non-empty check
		t.Fatal("no root certificates were loaded")
	}
}

// --- the release feed and the install -----------------------------------------

// releaseServer serves a whole signed release over TLS: feed, checksums,
// signature, certificate and the artifact itself.
type releaseServer struct {
	*httptest.Server
	feed Feed
}

// release describes a release to serve. The two binaries are separate so a test
// can publish checksums for one set of bytes and hand out another — the swapped-
// artifact case — and tamperSums edits the checksum file *after* it is signed.
type release struct {
	version    string
	binary     []byte // the bytes SHA256SUMS describes
	served     []byte // the bytes actually handed out; nil means binary
	tamperSums func([]byte)
}

func newReleaseServer(t *testing.T, ca *testCA, r release) *releaseServer {
	t.Helper()
	name := ArtifactName("linux", "amd64")
	digest := sha256.Sum256(r.binary)
	sums := []byte(hex.EncodeToString(digest[:]) + "  " + name + "\n")

	certPEM, key := ca.issue(t, leafOpts{identity: testIdentity, issuer: testIssuer})
	sig := signBlob(t, key, sums)
	if r.tamperSums != nil {
		r.tamperSums(sums)
	}
	served := r.served
	if served == nil {
		served = r.binary
	}

	mux := http.NewServeMux()
	rs := &releaseServer{}
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sums) })
	mux.HandleFunc("/SHA256SUMS.sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(sig)) })
	mux.HandleFunc("/SHA256SUMS.pem", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(certPEM) })
	mux.HandleFunc("/"+name, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(served) })
	mux.HandleFunc("/update-feed.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(rs.feed)
	})

	rs.Server = httptest.NewTLSServer(mux)
	t.Cleanup(rs.Close)
	rs.feed = Feed{
		Version:        r.version,
		ReleasedAt:     time.Now(),
		BaseURL:        rs.URL + "/",
		ChecksumsURL:   rs.URL + "/SHA256SUMS",
		SignatureURL:   rs.URL + "/SHA256SUMS.sig",
		CertificateURL: rs.URL + "/SHA256SUMS.pem",
	}
	return rs
}

// updaterFor wires an Updater at a release server, targeting a throwaway file
// that stands in for the installed binary.
func updaterFor(t *testing.T, rs *releaseServer, ca *testCA, current string) (*Updater, string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "pcsync")
	if err := os.WriteFile(target, []byte("the old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// httptest's TLS server has its own certificate; its client trusts it. Every
	// URL is still https, so the updater's own refusal to fetch over plaintext is
	// exercised rather than bypassed.
	u, err := New(Options{
		CurrentVersion: current,
		FeedURL:        rs.URL + "/update-feed.json",
		Verifier:       trust(ca, `^https://github\.com/guru-bharadwaj20/private-cloud/`),
		TargetPath:     target,
		GOOS:           "linux",
		GOARCH:         "amd64",
		HTTPClient:     rs.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return u, target
}

func TestApplyInstallsAVerifiedRelease(t *testing.T) {
	ca := newTestCA(t)
	binary := []byte("the new binary, honestly signed")
	rs := newReleaseServer(t, ca, release{version: "v1.3.0", binary: binary})
	u, target := updaterFor(t, rs, ca, "v1.2.0")

	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !rel.Newer || !rel.Comparable {
		t.Fatalf("v1.3.0 over v1.2.0 should be a newer, comparable release: %+v", rel)
	}
	res, err := u.Apply(context.Background(), rel)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.To != "v1.3.0" || res.From != "v1.2.0" {
		t.Errorf("result reports %s -> %s", res.From, res.To)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(binary) {
		t.Fatalf("target holds %q, want the downloaded binary", got)
	}
	leftovers := stagedFiles(t, filepath.Dir(target))
	if len(leftovers) != 0 {
		t.Errorf("staging files left behind: %v", leftovers)
	}
}

func TestApplyRefusesABadHash(t *testing.T) {
	ca := newTestCA(t)
	// The checksums are genuine and correctly signed — they simply do not
	// describe the bytes the server hands out. This is the swapped-artifact case:
	// everything about the signature checks out, and the download is still wrong.
	rs := newReleaseServer(t, ca, release{
		version: "v1.3.0",
		binary:  []byte("the binary the release actually signed"),
		served:  []byte("something else entirely"),
	})
	u, target := updaterFor(t, rs, ca, "v1.2.0")

	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Apply(context.Background(), rel); err == nil {
		t.Fatal("apply accepted a binary whose digest is not the signed one")
	} else if !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error %q does not name the digest mismatch", err)
	}
	assertUntouched(t, target)
}

func TestApplyRefusesABadSignature(t *testing.T) {
	ca := newTestCA(t)
	binary := []byte("the new binary")
	// Sign the checksums, then change them: exactly what a compromised release
	// host can do, and exactly what the signature is there to catch.
	rs := newReleaseServer(t, ca, release{version: "v1.3.0", binary: binary, tamperSums: func(sums []byte) {
		if sums[0] == 'f' {
			sums[0] = 'a'
		} else {
			sums[0] = 'f'
		}
	}})
	u, target := updaterFor(t, rs, ca, "v1.2.0")

	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Apply(context.Background(), rel); err == nil {
		t.Fatal("apply accepted checksums that were edited after signing")
	} else if !strings.Contains(err.Error(), "signature") {
		t.Fatalf("error %q does not name the signature", err)
	}
	assertUntouched(t, target)
}

func TestApplyRefusesADowngrade(t *testing.T) {
	ca := newTestCA(t)
	// Everything here is genuine and correctly signed. It is simply older — a
	// feed rolled back to a real, signed, known-bad release.
	rs := newReleaseServer(t, ca, release{version: "v1.1.0", binary: []byte("an older, genuinely signed binary")})
	u, target := updaterFor(t, rs, ca, "v1.2.0")

	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Newer {
		t.Fatal("v1.1.0 reported as newer than v1.2.0")
	}
	if _, err := u.Apply(context.Background(), rel); err == nil {
		t.Fatal("apply installed an older release")
	} else if !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("error %q does not name the downgrade", err)
	}
	assertUntouched(t, target)
}

func TestApplyRefusesToCompareADevBuild(t *testing.T) {
	ca := newTestCA(t)
	rs := newReleaseServer(t, ca, release{version: "v1.3.0", binary: []byte("a release binary")})
	u, target := updaterFor(t, rs, ca, "dev")

	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Comparable {
		t.Fatal("a dev build should not be comparable with a release tag")
	}
	if _, err := u.Apply(context.Background(), rel); err == nil {
		t.Fatal("apply overwrote a locally built binary")
	}
	assertUntouched(t, target)
}

func TestNewRefusesAnUpdaterThatCannotVerify(t *testing.T) {
	if _, err := New(Options{FeedURL: "https://example.invalid/f.json"}); err == nil {
		t.Fatal("an updater without a verifier was allowed")
	}
	v, err := FulcioTrust(`.`, testIssuer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{FeedURL: "http://example.invalid/f.json", Verifier: v}); err == nil {
		t.Fatal("a plaintext feed URL was allowed")
	}
}

// --- the pieces, on their own -------------------------------------------------

func TestReplaceBinaryIsAtomicAndLeavesNoDebris(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "pcsync")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, ".pcsync-update-123")
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(target, staged); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("target holds %q, want %q", got, "new")
	}
	// The staged file must have been consumed by the rename, not copied.
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staged file still exists after the swap")
	}
	// The Windows path moves the old binary aside; it must not leave it there
	// when the process holding it has let go.
	if _, err := os.Stat(target + ".old"); !os.IsNotExist(err) {
		t.Errorf("%s.old was left behind", target)
	}
}

func TestDigestFor(t *testing.T) {
	sums := []byte(
		"aa" + strings.Repeat("0", 62) + "  pcsync-linux-amd64\n" +
			"bb" + strings.Repeat("0", 62) + " *pcsync-windows-amd64.exe\n" +
			"cc" + strings.Repeat("0", 62) + "  ./pcsync-darwin-arm64\n")

	for name, want := range map[string]string{
		"pcsync-linux-amd64":       "aa" + strings.Repeat("0", 62),
		"pcsync-windows-amd64.exe": "bb" + strings.Repeat("0", 62),
		"pcsync-darwin-arm64":      "cc" + strings.Repeat("0", 62),
	} {
		got, err := digestFor(sums, name)
		if err != nil {
			t.Errorf("digestFor(%s): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("digestFor(%s) = %s, want %s", name, got, want)
		}
	}
	if _, err := digestFor(sums, "pcsync-plan9-386"); err == nil {
		t.Error("a target the release does not publish should be an error")
	}
	if _, err := digestFor([]byte("nothexatall  pcsync-linux-amd64\n"), "pcsync-linux-amd64"); err == nil {
		t.Error("a non-hex digest should be an error")
	}
}

func TestJoinURLRefusesToLeaveTheReleaseDirectory(t *testing.T) {
	base := "https://example.test/releases/download/v1.3.0/"
	got, err := joinURL(base, "pcsync-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got != base+"pcsync-linux-amd64" {
		t.Fatalf("joinURL = %s", got)
	}
	for _, bad := range []string{"../../../etc/passwd", "a/b", `a\b`} {
		if _, err := joinURL(base, bad); err == nil {
			t.Errorf("joinURL accepted %q", bad)
		}
	}
	if _, err := joinURL("http://example.test/", "pcsync-linux-amd64"); err == nil {
		t.Error("joinURL accepted a plaintext base URL")
	}
}

func TestArtifactNameMatchesTheReleaseScript(t *testing.T) {
	// build-release.sh writes exactly these names; if either side changes, the
	// updater downloads a 404 and this is where that is noticed.
	cases := map[[2]string]string{
		{"linux", "amd64"}:   "pcsync-linux-amd64",
		{"linux", "arm64"}:   "pcsync-linux-arm64",
		{"darwin", "arm64"}:  "pcsync-darwin-arm64",
		{"windows", "amd64"}: "pcsync-windows-amd64.exe",
	}
	for in, want := range cases {
		if got := ArtifactName(in[0], in[1]); got != want {
			t.Errorf("ArtifactName(%s, %s) = %s, want %s", in[0], in[1], got, want)
		}
	}
}

// --- helpers ------------------------------------------------------------------

func assertUntouched(t *testing.T, target string) {
	t.Helper()
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the target binary is gone after a failed update: %v", err)
	}
	if string(got) != "the old binary" {
		t.Fatalf("a failed update changed the binary: %q", got)
	}
	if leftovers := stagedFiles(t, filepath.Dir(target)); len(leftovers) != 0 {
		t.Errorf("a failed update left staging files behind: %v", leftovers)
	}
}

func stagedFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pcsync-update-") {
			out = append(out, e.Name())
		}
	}
	return out
}
