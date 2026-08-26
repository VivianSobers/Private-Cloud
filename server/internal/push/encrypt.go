package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// RFC 8291 message encryption, `aes128gcm` content encoding.
//
// The shape of it: the browser generated a P-256 keypair and a 16-byte auth
// secret when it subscribed and handed us the public halves (`p256dh`, `auth`).
// We generate a throwaway keypair per message, agree a shared secret with the
// browser's public key, run it through HKDF twice — once mixing in the auth
// secret and both public keys, once for the content key and nonce — and encrypt
// one record with AES-128-GCM.
//
// A fresh ephemeral keypair per message is not optional: the nonce is derived
// deterministically from the key material, so reusing a keypair across two
// messages to the same subscriber reuses the (key, nonce) pair, and a repeated
// nonce under GCM leaks the XOR of the plaintexts and destroys the
// authentication guarantee outright.

const (
	// saltLength and keyLength are fixed by the content encoding.
	saltLength = 16
	keyLength  = 16
	nonceSize  = 12
	// p256PointLength is an uncompressed P-256 point: 0x04 || X(32) || Y(32).
	p256PointLength = 65

	// maxPayloadLength is what a subscriber is guaranteed to accept. RFC 8291
	// requires push services to support 4096 octets of encrypted payload; the
	// overhead below is the record header plus the GCM tag plus the padding
	// delimiter, so this is the ceiling on the plaintext that fits inside it.
	maxPayloadLength = 4096 - (saltLength + 4 + 1 + p256PointLength) - 16 - 1
)

// Subscription is what a browser handed the API when it called
// PushManager.subscribe, stored verbatim since Phase 6.
type Subscription struct {
	Endpoint string
	// P256dh is the subscriber's public key, base64url, uncompressed point.
	P256dh string
	// Auth is the subscriber's 16-byte authentication secret, base64url.
	Auth string
}

// encrypt produces the request body for one notification: the `aes128gcm`
// record, header and all.
func encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	if len(plaintext) > maxPayloadLength {
		return nil, fmt.Errorf("payload is %d bytes, over the %d a subscriber must accept",
			len(plaintext), maxPayloadLength)
	}

	subKeyRaw, err := b64.DecodeString(sub.P256dh)
	if err != nil {
		return nil, fmt.Errorf("subscription key is not base64url: %w", err)
	}
	authSecret, err := b64.DecodeString(sub.Auth)
	if err != nil {
		return nil, fmt.Errorf("subscription auth secret is not base64url: %w", err)
	}
	if len(authSecret) != 16 {
		return nil, fmt.Errorf("subscription auth secret is %d bytes, want 16", len(authSecret))
	}

	curve := ecdh.P256()
	// NewPublicKey validates that the bytes are a point actually on the curve.
	// Skipping that is the classic invalid-curve attack: a crafted "public key"
	// from a hostile subscription would otherwise pull the shared secret onto a
	// weak subgroup and leak our ephemeral private key across a few messages.
	subKey, err := curve.NewPublicKey(subKeyRaw)
	if err != nil {
		return nil, fmt.Errorf("subscription key is not a valid P-256 point: %w", err)
	}

	ephemeral, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := ephemeral.ECDH(subKey)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}
	ephemeralPub := ephemeral.PublicKey().Bytes()

	salt := make([]byte, saltLength)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}

	key, nonce := deriveKeyAndNonce(shared, authSecret, salt, subKeyRaw, ephemeralPub)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// One record, so the padding delimiter is 0x02 ("last record"). A 0x01 here
	// tells the receiver another record follows and it will wait for one that
	// never comes.
	record := append(append([]byte{}, plaintext...), 0x02)
	ciphertext := aead.Seal(nil, nonce, record, nil)

	// Header: salt(16) || record size(4) || key id length(1) || key id.
	// The record size is what the receiver allocates, so it is the whole record
	// rather than the plaintext.
	header := make([]byte, 0, saltLength+4+1+len(ephemeralPub)+len(ciphertext))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, uint32(len(ciphertext)+saltLength+4+1+len(ephemeralPub)))
	header = append(header, byte(len(ephemeralPub)))
	header = append(header, ephemeralPub...)

	return append(header, ciphertext...), nil
}

// deriveKeyAndNonce is the RFC 8291 key schedule.
//
// Two HKDF passes, and the order matters. The first is salted with the
// subscriber's auth secret and mixes in BOTH public keys, which is what binds
// the derived key to this specific pair of parties — without it a shared secret
// alone would not authenticate who the other side is. The second, salted with
// the random per-message salt, produces the content key and the nonce.
func deriveKeyAndNonce(shared, authSecret, salt, subKey, ephemeralPub []byte) (key, nonce []byte) {
	// key_info = "WebPush: info" || 0x00 || ua_public || as_public
	keyInfo := make([]byte, 0, 14+len(subKey)+len(ephemeralPub))
	keyInfo = append(keyInfo, "WebPush: info"...)
	keyInfo = append(keyInfo, 0x00)
	keyInfo = append(keyInfo, subKey...)
	keyInfo = append(keyInfo, ephemeralPub...)

	ikm := hkdf(authSecret, shared, keyInfo, 32)

	key = hkdf(salt, ikm, []byte("Content-Encoding: aes128gcm\x00"), keyLength)
	nonce = hkdf(salt, ikm, []byte("Content-Encoding: nonce\x00"), nonceSize)
	return key, nonce
}

// hkdf is HKDF-SHA256 for outputs of at most one hash block, which is all any
// step above needs — extract, then a single-round expand.
func hkdf(salt, ikm, info []byte, length int) []byte {
	extract := hmac.New(sha256.New, salt)
	extract.Write(ikm)
	prk := extract.Sum(nil)

	expand := hmac.New(sha256.New, prk)
	expand.Write(info)
	expand.Write([]byte{0x01})
	return expand.Sum(nil)[:length]
}
