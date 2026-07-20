package auth

import (
	"strings"
	"testing"
)

func TestGenerateRecoveryCodesShape(t *testing.T) {
	codes, err := GenerateRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("got %d codes, want %d", len(codes), RecoveryCodeCount)
	}

	for _, c := range codes {
		if len(c) != 23 { // 20 chars + 3 dashes
			t.Errorf("code %q has length %d, want 23", c, len(c))
		}
		if strings.Count(c, "-") != 3 {
			t.Errorf("code %q should have 3 dashes", c)
		}
		// The alphabet deliberately excludes I, L, O and U because they are
		// the characters people misread off paper.
		for _, bad := range []string{"I", "L", "O", "U"} {
			if strings.Contains(c, bad) {
				t.Errorf("code %q contains ambiguous character %q", c, bad)
			}
		}
	}
}

func TestRecoveryCodesAreUnique(t *testing.T) {
	// A generator that repeats itself would quietly reduce the effective number
	// of codes, and the failure would be invisible until someone was locked out.
	codes, err := GenerateRecoveryCodes(200)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if seen[c] {
			t.Fatalf("duplicate code generated: %q", c)
		}
		seen[c] = true
	}
}

func TestHashAndVerifyRoundTrip(t *testing.T) {
	codes, err := GenerateRecoveryCodes(1)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	code := codes[0]

	hash, err := HashRecoveryCode(code)
	if err != nil {
		t.Fatalf("HashRecoveryCode: %v", err)
	}
	if !VerifyRecoveryCode(code, hash) {
		t.Fatal("a freshly hashed code failed to verify")
	}
	if strings.Contains(hash, NormalizeRecoveryCode(code)) {
		t.Fatal("the plaintext code appears inside its own hash")
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash is not in PHC argon2id format: %q", hash)
	}
}

func TestVerifyRejectsWrongCode(t *testing.T) {
	hash, err := HashRecoveryCode("ABCDE-FGHJK-MNPQR-STVWX")
	if err != nil {
		t.Fatalf("HashRecoveryCode: %v", err)
	}
	for _, wrong := range []string{
		"ABCDE-FGHJK-MNPQR-STVWY", // one character different
		"",
		"nonsense",
		"ABCDE-FGHJK-MNPQR", // truncated
	} {
		if VerifyRecoveryCode(wrong, hash) {
			t.Errorf("VerifyRecoveryCode accepted wrong code %q", wrong)
		}
	}
}

// Users retype these off paper. Case, dashes and stray whitespace must not
// matter, or a correct code gets rejected during an actual lockout.
func TestVerifyIsForgivingOfFormatting(t *testing.T) {
	const code = "ABCDE-FGHJK-MNPQR-STVWX"
	hash, err := HashRecoveryCode(code)
	if err != nil {
		t.Fatalf("HashRecoveryCode: %v", err)
	}

	for _, variant := range []string{
		"abcde-fghjk-mnpqr-stvwx",    // lowercase
		"ABCDEFGHJKMNPQRSTVWX",       // no dashes
		"  ABCDE-FGHJK-MNPQR-STVWX ", // surrounding whitespace
		"abcdefghjkmnpqrstvwx",       // both
	} {
		if !VerifyRecoveryCode(variant, hash) {
			t.Errorf("VerifyRecoveryCode rejected valid variant %q", variant)
		}
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	// Garbage in the hash column must fail closed, never panic and never pass.
	for _, bad := range []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$bcrypt$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$!!!notbase64!!!$aGFzaA",
		"$argon2id$v=99$m=65536,t=3,p=2$c2FsdA$aGFzaA", // wrong version
	} {
		if VerifyRecoveryCode("anything", bad) {
			t.Errorf("VerifyRecoveryCode accepted malformed hash %q", bad)
		}
	}
}

func TestSameCodeHashesDifferently(t *testing.T) {
	// Distinct salts per hash: identical codes must not produce identical
	// hashes, or a database leak reveals which users share a code.
	const code = "ABCDE-FGHJK-MNPQR-STVWX"
	h1, err := HashRecoveryCode(code)
	if err != nil {
		t.Fatalf("HashRecoveryCode: %v", err)
	}
	h2, err := HashRecoveryCode(code)
	if err != nil {
		t.Fatalf("HashRecoveryCode: %v", err)
	}
	if h1 == h2 {
		t.Fatal("identical hashes for the same code — salt is not random")
	}
	if !VerifyRecoveryCode(code, h1) || !VerifyRecoveryCode(code, h2) {
		t.Fatal("both hashes should verify against the original code")
	}
}

func TestNormalize(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"abc-def", "ABCDEF"},
		{" ABC DEF ", "ABCDEF"},
		{"A-B-C", "ABC"},
		{"", ""},
	} {
		if got := NormalizeRecoveryCode(tc.in); got != tc.want {
			t.Errorf("NormalizeRecoveryCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
