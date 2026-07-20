package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// RecoveryCodeCount is how many codes are issued per user. Ten is enough that
// losing a couple is survivable, few enough to print on one line each.
const RecoveryCodeCount = 10

// argon2id parameters. These are the RFC 9106 "second recommended" option
// (64 MiB, 3 passes), which is comfortable on the target hardware and far
// above what a GPU cracker finds convenient.
//
// Recovery codes are high-entropy (100 bits) so they are not brute-forceable
// regardless — argon2id here defends against a stolen database being useful
// even if a code ends up shorter or more structured in some future change.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Crockford-style base32 without padding: unambiguous when read off paper.
// The standard alphabet's I/L/O/U are the characters people misread.
var codeAlphabet = base32.NewEncoding("ABCDEFGHJKMNPQRSTVWXYZ0123456789").WithPadding(base32.NoPadding)

// GenerateRecoveryCodes returns freshly generated codes in plaintext. They are
// shown to the user EXACTLY ONCE; only hashes are persisted.
//
// Format: XXXXX-XXXXX-XXXXX-XXXXX (20 chars of base32 = 100 bits of entropy).
func GenerateRecoveryCodes(n int) ([]string, error) {
	codes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		raw := make([]byte, 13) // 13 bytes -> 21 base32 chars; we take 20
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generate recovery code: %w", err)
		}
		s := codeAlphabet.EncodeToString(raw)[:20]
		codes = append(codes, fmt.Sprintf("%s-%s-%s-%s", s[0:5], s[5:10], s[10:15], s[15:20]))
	}
	return codes, nil
}

// HashRecoveryCode produces a PHC-format argon2id hash for storage.
func HashRecoveryCode(code string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(NormalizeRecoveryCode(code)), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	// Standard PHC string, so the parameters travel with the hash and can be
	// raised later without invalidating existing codes.
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyRecoveryCode reports whether code matches the stored hash.
//
// Comparison is constant-time. It parses the parameters out of the stored hash
// rather than assuming the current constants, so codes hashed under older
// parameters keep working.
func VerifyRecoveryCode(code, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}

	var memory uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}

	got := argon2.IDKey([]byte(NormalizeRecoveryCode(code)), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// NormalizeRecoveryCode makes verification forgiving of how a human retypes a
// code off paper: case, dashes and surrounding whitespace are all irrelevant.
// Both hashing and verification run input through this, so the two agree.
func NormalizeRecoveryCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}
