package update

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// fulcioRootsPEM is the pinned Sigstore certificate authority. See the file's
// own header for where it comes from and what pinning costs.
//
//go:embed fulcio_roots.pem
var fulcioRootsPEM []byte

// Fulcio's certificate extensions, from the Sigstore OID allocation under
// 1.3.6.1.4.1.57264. Two spellings of the issuer exist in the wild: the original
// raw-string form, and the v2 form that is a properly DER-encoded UTF8String.
// Certificates in circulation carry one, the other, or both, so both are read.
var (
	oidIssuerV1 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	oidIssuerV2 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
)

// clockSkewAllowance bounds how far into the future a signing certificate may
// claim to have been minted. The chain is verified at the leaf's own NotBefore
// (see Verify), so without this a forged date is unbounded — with it, a
// certificate dated next year is rejected rather than humoured.
const clockSkewAllowance = 24 * time.Hour

// Verifier checks a Sigstore keyless signature over a blob. It holds the trust
// anchors and the identity it will accept; a zero Verifier trusts nothing.
type Verifier struct {
	// Roots and Intermediates are the certificate authority. Tests supply their
	// own; production uses FulcioTrust.
	Roots         *x509.CertPool
	Intermediates *x509.CertPool
	// Identity must match the certificate's SAN — the workflow that signed.
	Identity *regexp.Regexp
	// Issuer is the exact OIDC issuer that authenticated that workflow.
	Issuer string
	// Now supplies the current time; nil means time.Now. Tests set it.
	Now func() time.Time
}

// FulcioTrust returns a Verifier wired to the pinned Sigstore CA, accepting only
// signatures whose certificate identity matches identity and whose OIDC issuer
// is issuer.
func FulcioTrust(identity, issuer string) (*Verifier, error) {
	re, err := regexp.Compile(identity)
	if err != nil {
		return nil, fmt.Errorf("update: identity pattern: %w", err)
	}
	certs, err := parseCerts(fulcioRootsPEM)
	if err != nil {
		return nil, fmt.Errorf("update: embedded Fulcio roots are unreadable: %w", err)
	}
	if len(certs) == 0 {
		return nil, errors.New("update: embedded Fulcio roots are empty")
	}
	v := &Verifier{
		Roots:         x509.NewCertPool(),
		Intermediates: x509.NewCertPool(),
		Identity:      re,
		Issuer:        issuer,
	}
	for _, c := range certs {
		// A self-signed certificate is the root; anything else is an issuing
		// intermediate. Sorting them by shape rather than by position in the file
		// means the bundle can be refreshed without re-teaching this function.
		if c.CheckSignatureFrom(c) == nil {
			v.Roots.AddCert(c)
		} else {
			v.Intermediates.AddCert(c)
		}
	}
	return v, nil
}

// Verify checks that sigB64 is a valid signature over payload, made by a
// certificate that chains to the trusted CA and carries the expected identity
// and issuer.
//
// The chain is validated at the leaf certificate's own NotBefore, not at the
// current time. Fulcio signing certificates live about ten minutes, so verifying
// "now" would reject every release older than lunchtime. Cosign solves this
// properly by taking the signing time from the Rekor transparency log; this
// updater does not talk to Rekor (see the package comment), so it takes the
// certificate's word for when it was minted and bounds that claim with
// clockSkewAllowance. That is a real, named gap, not an oversight.
func (v *Verifier) Verify(payload, certPEM []byte, sigB64 string) error {
	if v == nil || v.Roots == nil || v.Identity == nil {
		return errors.New("update: no trust configured")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return fmt.Errorf("update: signature is not base64: %w", err)
	}
	certs, err := parseCerts(certPEM)
	if err != nil {
		return err
	}
	if len(certs) == 0 {
		return errors.New("update: signing certificate is empty")
	}
	leaf := certs[0]

	// Any certificates shipped alongside the leaf are chain material, never
	// additional roots — they are pooled as intermediates so they still have to
	// lead back to the pinned root.
	inter := x509.NewCertPool()
	if v.Intermediates != nil {
		inter = v.Intermediates.Clone()
	}
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}

	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	if leaf.NotBefore.After(now().Add(clockSkewAllowance)) {
		return fmt.Errorf("update: signing certificate is dated %s, in the future", leaf.NotBefore.Format(time.RFC3339))
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         v.Roots,
		Intermediates: inter,
		CurrentTime:   leaf.NotBefore.Add(time.Second),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}); err != nil {
		return fmt.Errorf("update: signing certificate does not chain to the pinned Sigstore CA: %w", err)
	}
	if err := v.checkIdentity(leaf); err != nil {
		return err
	}
	if err := checkSignature(leaf, payload, sig); err != nil {
		return fmt.Errorf("update: signature does not match the release checksums: %w", err)
	}
	return nil
}

// checkIdentity enforces who signed. Both halves matter: the SAN says which
// workflow in which repository at which tag, and the issuer says which identity
// provider vouched for it. Checking the SAN alone would accept a certificate
// minted by any issuer willing to assert that string.
func (v *Verifier) checkIdentity(leaf *x509.Certificate) error {
	var names []string
	for _, u := range leaf.URIs {
		names = append(names, u.String())
	}
	names = append(names, leaf.EmailAddresses...)
	names = append(names, leaf.DNSNames...)

	matched := false
	for _, n := range names {
		if v.Identity.MatchString(n) {
			matched = true
			break
		}
	}
	if !matched {
		return fmt.Errorf("update: signing identity %q is not one this build accepts", strings.Join(names, ", "))
	}
	issuer, ok := certIssuer(leaf)
	if !ok {
		return errors.New("update: signing certificate names no OIDC issuer")
	}
	if issuer != v.Issuer {
		return fmt.Errorf("update: signing identity was issued by %q, not %q", issuer, v.Issuer)
	}
	return nil
}

// certIssuer reads the OIDC issuer out of a Fulcio certificate, accepting both
// the raw v1 extension and the DER-encoded v2 one.
func certIssuer(c *x509.Certificate) (string, bool) {
	for _, ext := range c.Extensions {
		switch {
		case ext.Id.Equal(oidIssuerV2):
			var s string
			if _, err := asn1.Unmarshal(ext.Value, &s); err == nil && s != "" {
				return s, true
			}
		case ext.Id.Equal(oidIssuerV1):
			if s := strings.TrimSpace(string(ext.Value)); s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// checkSignature verifies sig over payload with the certificate's public key,
// pairing each key type with the digest cosign uses for it.
func checkSignature(leaf *x509.Certificate, payload, sig []byte) error {
	var algo x509.SignatureAlgorithm
	switch leaf.PublicKey.(type) {
	case *ecdsa.PublicKey:
		algo = x509.ECDSAWithSHA256
	case *rsa.PublicKey:
		algo = x509.SHA256WithRSA
	case ed25519.PublicKey:
		algo = x509.PureEd25519
	default:
		return fmt.Errorf("unsupported signing key type %T", leaf.PublicKey)
	}
	return leaf.CheckSignature(algo, payload, sig)
}

// parseCerts reads every certificate in a PEM blob, ignoring any surrounding
// prose (the embedded roots file leads with a note about where it came from).
func parseCerts(data []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("update: parse certificate: %w", err)
		}
		out = append(out, c)
	}
	return out, nil
}
