// Package push delivers Web Push notifications to the browsers that registered
// for them.
//
// This is the half that was missing. The API has stored subscriptions since
// Phase 6 — `POST /devices/{id}/push` — but nothing published an application
// server key, so no PWA could call PushManager.subscribe in the first place, and
// nothing would have delivered if one had. The route was the only one in the
// repository served with no client.
//
// What push is NOT, and the design follows from it: push is a latency
// optimisation, never a correctness requirement. A client that registers nothing
// — or whose subscription has expired, or whose browser vendor is unreachable —
// discovers every change by polling GET /changes, exactly as before. So every
// failure here is logged and dropped rather than retried into a queue or
// surfaced to a caller: a notification that did not arrive costs a few seconds
// of staleness, and building a delivery guarantee for that would be building the
// most complicated part of the system for the least important one.
//
// Two RFCs, both implemented on the standard library:
//
//   - RFC 8291, Message Encryption for Web Push: ECDH on P-256 to a key the
//     browser generated, HKDF to a content key, AES-128-GCM.
//   - RFC 8292, VAPID: an ES256 JWT that identifies this server to the browser
//     vendor's push service.
//
// The payload is encrypted to a key only the subscriber's browser holds, so the
// push service — Google's, Mozilla's, Apple's — forwards bytes it cannot read.
// That matters more here than in most deployments: the whole point of this
// system is that content does not leave your infrastructure, and a notification
// saying which file changed would leak exactly that to a third party if it went
// in clear.
package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// vapidTTL is how long a signed token stays valid. RFC 8292 caps it at 24 hours;
// twelve keeps a comfortable margin against clock skew in either direction
// without minting a token per request.
const vapidTTL = 12 * time.Hour

// b64 is the unpadded base64url every one of these formats uses — JWT segments,
// the application server key, and the subscription keys the browser sends.
var b64 = base64.RawURLEncoding

// Keys is the application server's VAPID identity: one P-256 keypair, generated
// once per deployment and then stable.
//
// Stable because the public half is baked into every subscription a browser has
// created: PushManager.subscribe binds the subscription to the applicationServer
// Key it was given, and a push signed by a different key is rejected. Rotating
// therefore invalidates every existing subscription, which is why this is
// configuration rather than something regenerated at startup.
type Keys struct {
	private *ecdsa.PrivateKey
	// Public is the uncompressed P-256 point, base64url encoded — the exact
	// string a browser expects as applicationServerKey.
	Public string
	// Subject identifies who to contact about this server, as a mailto: or
	// https: URL. Push services treat it as the operator's contact for abuse.
	Subject string
}

// GenerateKeys mints a new VAPID keypair, returning both halves base64url
// encoded for an operator to paste into configuration.
func GenerateKeys() (publicKey, privateKey string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	pub := elliptic.Marshal(elliptic.P256(), key.PublicKey.X, key.PublicKey.Y) //nolint:staticcheck // the wire format IS this encoding
	return b64.EncodeToString(pub), b64.EncodeToString(key.D.FillBytes(make([]byte, 32))), nil
}

// LoadKeys rebuilds the keypair from configuration.
//
// The public half is derived from the private one rather than read alongside it,
// so a configuration holding a mismatched pair is impossible by construction —
// the failure it would otherwise cause is every push being rejected as
// unauthorised by a service that will not say why.
func LoadKeys(privateKey, subject string) (*Keys, error) {
	raw, err := b64.DecodeString(strings.TrimSpace(privateKey))
	if err != nil {
		return nil, fmt.Errorf("VAPID private key is not base64url: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("VAPID private key is %d bytes, want 32", len(raw))
	}
	if err := validSubject(subject); err != nil {
		return nil, err
	}

	d := new(big.Int).SetBytes(raw)
	curve := elliptic.P256()
	if d.Sign() == 0 || d.Cmp(curve.Params().N) >= 0 {
		return nil, fmt.Errorf("VAPID private key is not a valid P-256 scalar")
	}

	key := &ecdsa.PrivateKey{D: d}
	key.PublicKey.Curve = curve
	key.PublicKey.X, key.PublicKey.Y = curve.ScalarBaseMult(raw)

	pub := elliptic.Marshal(curve, key.PublicKey.X, key.PublicKey.Y) //nolint:staticcheck // the wire format IS this encoding
	return &Keys{private: key, Public: b64.EncodeToString(pub), Subject: subject}, nil
}

// validSubject rejects anything a push service would not accept as a contact.
// Checked at load rather than at send, because a bad subject means every
// notification fails and the moment to find that out is startup.
func validSubject(subject string) error {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return fmt.Errorf("VAPID subject is required: a mailto: or https: URL identifying the operator")
	}
	u, err := url.Parse(subject)
	if err != nil {
		return fmt.Errorf("VAPID subject is not a URL: %w", err)
	}
	switch u.Scheme {
	case "mailto", "https":
		return nil
	default:
		return fmt.Errorf("VAPID subject must be a mailto: or https: URL (got %q)", subject)
	}
}

// authorization builds the `Authorization: vapid ...` header for one push
// endpoint.
//
// The audience is the endpoint's ORIGIN, not the endpoint itself. The full URL
// contains the subscription identifier, which is a secret shared between the
// browser and its push service; signing it into a token would be putting it
// somewhere it does not need to be, and the RFC asks for the origin regardless.
func (k *Keys) authorization(endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("push endpoint is not a URL: %w", err)
	}
	audience := u.Scheme + "://" + u.Host

	token, err := k.sign(audience, now)
	if err != nil {
		return "", err
	}
	return "vapid t=" + token + ", k=" + k.Public, nil
}

// sign produces the ES256 JWT. Written out rather than pulled from a JWT library
// because it is three fixed fields and a signature: the only variability a
// library would absorb is the part that must not vary.
func (k *Keys) sign(audience string, now time.Time) (string, error) {
	header := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))

	claims, err := json.Marshal(map[string]any{
		"aud": audience,
		"exp": now.Add(vapidTTL).Unix(),
		"sub": k.Subject,
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + b64.EncodeToString(claims)

	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, k.private, digest[:])
	if err != nil {
		return "", err
	}

	// JWS wants the raw r||s pair, each left-padded to the curve size — NOT the
	// ASN.1 SEQUENCE crypto/ecdsa hands back from its higher-level API. A DER
	// signature here is accepted by nothing and fails as a bare 401.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	return signingInput + "." + b64.EncodeToString(sig), nil
}

// verify reports whether a token this package produced carries a good signature
// for the given public key. Used by the tests; kept here so the encoding is
// asserted against the same constants that wrote it.
func verify(token, publicKey string) (bool, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false, fmt.Errorf("token has %d segments, want 3", len(parts))
	}
	raw, err := b64.DecodeString(parts[2])
	if err != nil || len(raw) != 64 {
		return false, fmt.Errorf("signature is not 64 raw bytes")
	}
	pub, err := b64.DecodeString(publicKey)
	if err != nil {
		return false, err
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), pub) //nolint:staticcheck // matches Marshal above
	if x == nil {
		return false, fmt.Errorf("public key is not a P-256 point")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, digest[:],
		new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:])), nil
}
