package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// subscriber stands in for a browser: it holds the keypair and auth secret that
// PushManager.subscribe would have generated, and it can decrypt what the server
// sends.
//
// Decryption is written from the RECEIVER's side of RFC 8291 — parsing the
// record header off the wire and re-deriving the key from the ephemeral public
// key it finds there — rather than by calling back into the encrypt path. That
// is what makes the round trip evidence: a mistake in the key schedule, the
// header layout or the padding delimiter has to be made identically in two
// places written from opposite ends of the spec to go unnoticed.
type subscriber struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newSubscriber(t *testing.T) *subscriber {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, auth); err != nil {
		t.Fatal(err)
	}
	return &subscriber{priv: priv, auth: auth}
}

func (s *subscriber) subscription(endpoint string) Subscription {
	return Subscription{
		Endpoint: endpoint,
		P256dh:   b64.EncodeToString(s.priv.PublicKey().Bytes()),
		Auth:     b64.EncodeToString(s.auth),
	}
}

func (s *subscriber) decrypt(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < saltLength+4+1 {
		t.Fatalf("body is %d bytes, too short for a record header", len(body))
	}
	salt := body[:saltLength]
	declared := binary.BigEndian.Uint32(body[saltLength:])
	idLen := int(body[saltLength+4])
	if idLen != p256PointLength {
		t.Fatalf("key id is %d bytes, want an uncompressed P-256 point", idLen)
	}
	if int(declared) != len(body) {
		t.Errorf("record size header says %d, body is %d", declared, len(body))
	}

	off := saltLength + 4 + 1
	serverPubRaw := body[off : off+idLen]
	ciphertext := body[off+idLen:]

	serverPub, err := ecdh.P256().NewPublicKey(serverPubRaw)
	if err != nil {
		t.Fatalf("server ephemeral key is not on the curve: %v", err)
	}
	shared, err := s.priv.ECDH(serverPub)
	if err != nil {
		t.Fatal(err)
	}

	key, nonce := deriveKeyAndNonce(shared, s.auth, salt, s.priv.PublicKey().Bytes(), serverPubRaw)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	record, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(record) == 0 {
		t.Fatal("record is empty; the padding delimiter is missing")
	}
	if record[len(record)-1] != 0x02 {
		t.Errorf("padding delimiter is %#x, want 0x02 for a single final record", record[len(record)-1])
	}
	return record[:len(record)-1]
}

func TestEncryptedPayloadRoundTrips(t *testing.T) {
	sub := newSubscriber(t)
	want := []byte(`{"type":"changes","seq":41}`)

	body, err := encrypt(sub.subscription("https://push.example/x"), want)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if got := sub.decrypt(t, body); string(got) != string(want) {
		t.Errorf("round trip gave %q, want %q", got, want)
	}
}

// The nonce is derived from the key material, so a repeated ephemeral keypair
// repeats the (key, nonce) pair — which under GCM leaks the XOR of the two
// plaintexts and voids authentication. This asserts the one property that
// prevents it.
func TestEachMessageUsesAFreshEphemeralKey(t *testing.T) {
	sub := newSubscriber(t)
	s := sub.subscription("https://push.example/x")

	seen := map[string]bool{}
	salts := map[string]bool{}
	for i := 0; i < 25; i++ {
		body, err := encrypt(s, []byte("same payload every time"))
		if err != nil {
			t.Fatal(err)
		}
		off := saltLength + 4 + 1
		keyID := string(body[off : off+p256PointLength])
		if seen[keyID] {
			t.Fatal("an ephemeral public key repeated across messages")
		}
		seen[keyID] = true

		salt := string(body[:saltLength])
		if salts[salt] {
			t.Fatal("a salt repeated across messages")
		}
		salts[salt] = true
	}
}

// A hostile subscription could offer a "public key" that is not on the curve, to
// pull the shared secret onto a small subgroup and leak our private scalar. The
// standard library refuses it; this pins that we let it.
func TestSubscriptionKeyMustBeOnTheCurve(t *testing.T) {
	bad := make([]byte, p256PointLength)
	bad[0] = 0x04 // uncompressed marker, then 64 bytes of zeroes: not a point

	_, err := encrypt(Subscription{
		Endpoint: "https://push.example/x",
		P256dh:   b64.EncodeToString(bad),
		Auth:     b64.EncodeToString(make([]byte, 16)),
	}, []byte("hello"))
	if err == nil {
		t.Fatal("a point off the curve was accepted")
	}
	if !strings.Contains(err.Error(), "P-256") {
		t.Errorf("error should name the curve, got %v", err)
	}
}

func TestMalformedSubscriptionsAreRejected(t *testing.T) {
	good := newSubscriber(t).subscription("https://push.example/x")
	cases := map[string]Subscription{
		"key not base64":     {Endpoint: good.Endpoint, P256dh: "!!!!", Auth: good.Auth},
		"auth not base64":    {Endpoint: good.Endpoint, P256dh: good.P256dh, Auth: "!!!!"},
		"auth wrong length":  {Endpoint: good.Endpoint, P256dh: good.P256dh, Auth: b64.EncodeToString([]byte("short"))},
		"key wrong length":   {Endpoint: good.Endpoint, P256dh: b64.EncodeToString([]byte{4, 1, 2}), Auth: good.Auth},
		"empty subscription": {},
	}
	for name, sub := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := encrypt(sub, []byte("x")); err == nil {
				t.Error("accepted a malformed subscription")
			}
		})
	}
}

// A payload over what a push service must accept is refused here rather than
// discovered as a 413 from a third party.
func TestOversizePayloadIsRefusedLocally(t *testing.T) {
	sub := newSubscriber(t).subscription("https://push.example/x")
	if _, err := encrypt(sub, make([]byte, maxPayloadLength+1)); err == nil {
		t.Fatal("an oversize payload was accepted")
	}
	if _, err := encrypt(sub, make([]byte, maxPayloadLength)); err != nil {
		t.Fatalf("a payload at the documented limit was refused: %v", err)
	}
}

func TestVAPIDKeysRoundTripThroughConfiguration(t *testing.T) {
	pub, priv, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys(priv, "mailto:ops@example.com")
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}
	// Derived, not stored — a mismatched pair should be impossible.
	if keys.Public != pub {
		t.Errorf("public key derived from the private half is %q, want %q", keys.Public, pub)
	}
}

func TestVAPIDRejectsBadConfiguration(t *testing.T) {
	_, priv, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ key, subject string }{
		"not base64":     {"!!!!", "mailto:a@b.c"},
		"wrong length":   {b64.EncodeToString([]byte("short")), "mailto:a@b.c"},
		"zero scalar":    {b64.EncodeToString(make([]byte, 32)), "mailto:a@b.c"},
		"no subject":     {priv, ""},
		"bad subject":    {priv, "ftp://example.com"},
		"opaque subject": {priv, "not a url at all"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadKeys(tc.key, tc.subject); err == nil {
				t.Error("bad configuration was accepted")
			}
		})
	}
}

// The token has to verify under the published key, carry the endpoint's ORIGIN
// as its audience, and be a raw r||s signature rather than the ASN.1 one
// crypto/ecdsa returns by default.
func TestVAPIDTokenIsVerifiableAndScopedToTheOrigin(t *testing.T) {
	_, priv, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys(priv, "mailto:ops@example.com")
	if err != nil {
		t.Fatal(err)
	}

	header, err := keys.authorization("https://fcm.googleapis.com/fcm/send/abc123?secret=x", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(header, "vapid t=") || !strings.Contains(header, ", k="+keys.Public) {
		t.Fatalf("authorization header is malformed: %q", header)
	}
	token := strings.TrimPrefix(strings.Split(header, ", k=")[0], "vapid t=")

	ok, err := verify(token, keys.Public)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("the token does not verify under its own public key")
	}

	var claims struct {
		Aud string `json:"aud"`
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	raw, err := b64.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	// The subscription id and its query string are a secret between the browser
	// and its push service. They must not end up signed into a token.
	if claims.Aud != "https://fcm.googleapis.com" {
		t.Errorf("aud = %q, want just the origin", claims.Aud)
	}
	if claims.Sub != "mailto:ops@example.com" {
		t.Errorf("sub = %q", claims.Sub)
	}
	if d := time.Until(time.Unix(claims.Exp, 0)); d > 24*time.Hour {
		t.Errorf("exp is %v away, over the 24h RFC 8292 allows", d)
	}
}

// A subscription the push service has forgotten must be distinguishable, because
// it is the one failure worth acting on: the row should be deleted rather than
// retried forever.
func TestGoneSubscriptionIsReportedDistinctly(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		t.Cleanup(srv.Close)

		sender := newTestSender(t)
		err := sender.Send(context.Background(), newSubscriber(t).subscription(srv.URL), []byte("x"))
		if !errors.Is(err, ErrGone) {
			t.Errorf("status %d gave %v, want ErrGone", code, err)
		}
	}
}

func TestSendPostsAnEncryptedRequest(t *testing.T) {
	sub := newSubscriber(t)
	var (
		gotAuth     string
		gotEncoding string
		gotTTL      string
		gotBody     []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotEncoding = r.Header.Get("Content-Encoding")
		gotTTL = r.Header.Get("TTL")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	sender := newTestSender(t)
	want := []byte(`{"type":"changes"}`)
	if err := sender.Send(context.Background(), sub.subscription(srv.URL), want); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !strings.HasPrefix(gotAuth, "vapid t=") {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotEncoding != "aes128gcm" {
		t.Errorf("Content-Encoding = %q, want aes128gcm", gotEncoding)
	}
	if gotTTL == "" {
		t.Error("no TTL header; a push service may drop an undeliverable message immediately")
	}
	// The body that actually went over the wire decrypts to what we sent, which
	// is the end-to-end claim: the push service is forwarding bytes it cannot
	// read.
	if got := sub.decrypt(t, gotBody); string(got) != string(want) {
		t.Errorf("wire body decrypts to %q, want %q", got, want)
	}
}

func TestSendReportsServiceErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad vapid token"))
	}))
	t.Cleanup(srv.Close)

	err := newTestSender(t).Send(context.Background(), newSubscriber(t).subscription(srv.URL), []byte("x"))
	if err == nil {
		t.Fatal("a 400 was treated as success")
	}
	if errors.Is(err, ErrGone) {
		t.Error("a 400 is not a gone subscription")
	}
	if !strings.Contains(err.Error(), "bad vapid token") {
		t.Errorf("the service's explanation was dropped: %v", err)
	}
}

func newTestSender(t *testing.T) *Sender {
	t.Helper()
	_, priv, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys(priv, "mailto:ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return NewSender(keys)
}
