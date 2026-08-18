package config

import (
	"strings"
	"testing"
)

// validEnv sets the environment a successful Load needs, so a test about one
// setting is not also asserting the defaults of every other.
//
// PC_BLOB_PATH is set explicitly because its default, "/data/blobs", is
// absolute only on the deployment target. filepath.IsAbs answers a question
// about the *host*: on Windows a rooted path with no drive letter is relative,
// so the default fails its own validation there. The validation is right — a
// drive-relative blob root is a real misconfiguration — so the test supplies a
// path that is absolute wherever it runs rather than weakening the check.
func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PC_DATABASE_URL", "postgres://u:p@localhost:5432/pc")
	t.Setenv("PC_BLOB_PATH", t.TempDir())
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("PC_DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error when PC_DATABASE_URL is unset, got nil")
	}
}

func TestLoadRejectsNonPostgresURL(t *testing.T) {
	// A bare host is the realistic copy-paste mistake, and pgx would otherwise
	// fail with a much less obvious error at connect time.
	t.Setenv("PC_DATABASE_URL", "db.internal:5432")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a non-postgres URL, got nil")
	}
}

func TestLoadDefaults(t *testing.T) {
	validEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", c.HTTPAddr)
	}
	if c.Env != "dev" {
		t.Errorf("Env = %q, want dev", c.Env)
	}
	if !c.MigrateOnStart {
		t.Error("MigrateOnStart should default to true")
	}
}

func TestLoadRejectsBadEnum(t *testing.T) {
	validEnv(t)

	for _, tc := range []struct{ key, val string }{
		{"PC_ENV", "staging"},
		{"PC_LOG_FORMAT", "xml"},
		{"PC_LOG_LEVEL", "verbose"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Errorf("expected an error for %s=%s, got nil", tc.key, tc.val)
			}
		})
	}
}

func TestLoadRejectsMinConnsAboveMax(t *testing.T) {
	validEnv(t)
	t.Setenv("PC_DB_MIN_CONNS", "50")
	t.Setenv("PC_DB_MAX_CONNS", "10")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error when min conns exceeds max, got nil")
	}
}

// The password must never reach the logs. This is the test that stops a future
// refactor from quietly logging the raw config.
func TestRedactedHidesPassword(t *testing.T) {
	c := &Config{DatabaseURL: "postgres://privatecloud:sup3rs3cret@postgres:5432/privatecloud"}

	got, ok := c.Redacted()["database_url"].(string)
	if !ok {
		t.Fatal("database_url missing from Redacted()")
	}
	if strings.Contains(got, "sup3rs3cret") {
		t.Fatalf("password leaked into redacted output: %q", got)
	}
	// Still useful for debugging: user, host and database survive.
	for _, want := range []string{"privatecloud", "postgres:5432"} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted URL %q lost useful context %q", got, want)
		}
	}
}

// RPID with a scheme or port is the classic WebAuthn misconfiguration, and the
// browser-side error it produces is famously unhelpful. Fail at startup instead.
func TestLoadRejectsMalformedRPID(t *testing.T) {
	validEnv(t)

	for _, bad := range []string{
		"https://cloud.example.ts.net",
		"cloud.example.ts.net:443",
		"cloud.example.ts.net/app",
	} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("PC_WEBAUTHN_RPID", bad)
			if _, err := Load(); err == nil {
				t.Errorf("expected an error for RPID %q, got nil", bad)
			}
		})
	}
}

func TestLoadAcceptsBareRPID(t *testing.T) {
	validEnv(t)
	t.Setenv("PC_WEBAUTHN_RPID", "cloud.example.ts.net")

	if _, err := Load(); err != nil {
		t.Fatalf("a bare domain RPID should be accepted: %v", err)
	}
}

// Origins are the mirror image of RPID: they must carry a scheme.
func TestLoadRejectsSchemelessOrigin(t *testing.T) {
	validEnv(t)
	t.Setenv("PC_WEBAUTHN_ORIGINS", "cloud.example.ts.net")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error for an origin without a scheme")
	}
}

func TestLoadParsesOriginList(t *testing.T) {
	validEnv(t)
	// Trailing comma and stray spaces are the realistic hand-edited .env.
	t.Setenv("PC_WEBAUTHN_ORIGINS", "https://a.ts.net, https://b.ts.net,")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.WebAuthnOrigins) != 2 {
		t.Fatalf("got %d origins, want 2: %v", len(c.WebAuthnOrigins), c.WebAuthnOrigins)
	}
	if c.WebAuthnOrigins[1] != "https://b.ts.net" {
		t.Errorf("origin not trimmed: %q", c.WebAuthnOrigins[1])
	}
}

// Shipping session cookies in the clear in production must be impossible to do
// by accident.
func TestLoadRejectsInsecureCookiesInProd(t *testing.T) {
	validEnv(t)
	t.Setenv("PC_ENV", "prod")
	t.Setenv("PC_COOKIE_SECURE", "false")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error for PC_COOKIE_SECURE=false with PC_ENV=prod")
	}
}

func TestRedactURLWithoutPassword(t *testing.T) {
	// A URL with no password must pass through unchanged rather than being
	// mangled into something misleading.
	const in = "postgres://user@host:5432/db"
	if got := redactURL(in); got != in {
		t.Errorf("redactURL(%q) = %q, want unchanged", in, got)
	}
}
