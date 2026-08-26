package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// --- video thumbnails (Phase 5) -------------------------------------------

// Absent by default, and deliberately not inferred from PATH: ffmpeg is on a
// great many machines as somebody else's dependency, and this switch turns on
// the largest hostile-input surface in the system.
func TestVideoThumbnailsAreOffByDefault(t *testing.T) {
	validEnv(t)
	t.Setenv("PC_FFMPEG_PATH", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Media.VideoThumbnailsEnabled() {
		t.Error("video thumbnails are enabled with no PC_FFMPEG_PATH set")
	}
	if c.Media.FFmpegPath != "" {
		t.Errorf("FFmpegPath = %q, want empty", c.Media.FFmpegPath)
	}
}

func TestLoadAcceptsAnFFmpegPath(t *testing.T) {
	validEnv(t)
	// An absolute path and a bare command name are both legitimate: one names a
	// binary, the other asks PATH for it.
	for _, val := range []string{filepath.Join(t.TempDir(), "ffmpeg"), "ffmpeg"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("PC_FFMPEG_PATH", val)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !c.Media.VideoThumbnailsEnabled() || c.Media.FFmpegPath != val {
				t.Errorf("FFmpegPath = %q, enabled = %v", c.Media.FFmpegPath, c.Media.VideoThumbnailsEnabled())
			}
		})
	}
}

// A relative path to an executable resolves against whatever the working
// directory happens to be — which is how the wrong binary gets run.
func TestLoadRejectsARelativeFFmpegPath(t *testing.T) {
	validEnv(t)
	t.Setenv("PC_FFMPEG_PATH", "./bin/ffmpeg")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a relative PC_FFMPEG_PATH, got nil")
	}
}

// A missing binary must NOT stop the worker: it degrades to no video
// thumbnails, which is the state the default is already in, and refusing to
// start would take OCR, tagging and embedding down with it.
func TestLoadAcceptsAnFFmpegPathThatDoesNotExist(t *testing.T) {
	validEnv(t)
	t.Setenv("PC_FFMPEG_PATH", filepath.Join(t.TempDir(), "definitely-not-here"))
	if _, err := Load(); err != nil {
		t.Fatalf("a missing binary should not fail startup: %v", err)
	}
}

// The startup log is where "configured but missing" is diagnosed, so the
// setting has to appear in it.
func TestRedactedReportsVideoThumbnails(t *testing.T) {
	validEnv(t)
	t.Setenv("PC_FFMPEG_PATH", "ffmpeg")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	r := c.Redacted()
	if r["video_thumbnails"] != true {
		t.Errorf("video_thumbnails = %v, want true", r["video_thumbnails"])
	}
	if r["ffmpeg_path"] != "ffmpeg" {
		t.Errorf("ffmpeg_path = %v, want ffmpeg", r["ffmpeg_path"])
	}
}

// --- cold tier ---------------------------------------------------------------

// Off is the default, and off has to be reachable with no cold-tier variables
// set at all — every existing deployment is in exactly that state.
func TestColdTierIsOffByDefault(t *testing.T) {
	validEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Cold.ColdTierEnabled() {
		t.Error("the cold tier should be off unless PC_COLD_TIER_ENABLED is set")
	}
}

// The refusals that matter. Every one of these decides WHERE content goes, and
// a cold tier pointed at the wrong bucket is not a condition to discover later:
// by the time it is visible, the bytes have already moved.
func TestEnablingTheColdTierRefusesAnIncompleteConfiguration(t *testing.T) {
	full := map[string]string{
		"PC_COLD_TIER_ENABLED":  "true",
		"PC_COLD_S3_ENDPOINT":   "https://s3.example.invalid",
		"PC_COLD_S3_BUCKET":     "cold",
		"PC_COLD_S3_ACCESS_KEY": "key",
		"PC_COLD_S3_SECRET_KEY": "secret",
	}
	for _, missing := range []string{
		"PC_COLD_S3_ENDPOINT", "PC_COLD_S3_BUCKET",
		"PC_COLD_S3_ACCESS_KEY", "PC_COLD_S3_SECRET_KEY",
	} {
		t.Run("without "+missing, func(t *testing.T) {
			validEnv(t)
			for k, v := range full {
				if k == missing {
					continue
				}
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatalf("enabling the cold tier without %s should refuse to start", missing)
			}
		})
	}
}

// A bucket name with no endpoint, while the tier is off, is half a
// configuration somebody stopped writing. Naming it now is cheaper than
// discovering it at the moment the tier is switched on.
func TestHalfAColdTierConfigurationIsNamedWhileItIsStillInert(t *testing.T) {
	validEnv(t)
	t.Setenv("PC_COLD_S3_BUCKET", "cold")
	if _, err := Load(); err == nil {
		t.Fatal("a bucket with no endpoint should be reported, not started past")
	}
}

// The policy defaults are the conservative ones, and they are the SAME numbers
// tiering.DefaultPolicy uses rather than a second set that could drift apart.
func TestColdTierPolicyDefaultsAreConservative(t *testing.T) {
	validEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Cold.MinAge != 90*24*time.Hour || c.Cold.MinIdle != 90*24*time.Hour {
		t.Errorf("MinAge/MinIdle = %v/%v, want 90 days each", c.Cold.MinAge, c.Cold.MinIdle)
	}
	if c.Cold.MinSize != 1<<20 {
		t.Errorf("MinSize = %d, want 1 MiB", c.Cold.MinSize)
	}
	if c.Cold.Batch != 100 {
		t.Errorf("Batch = %d, want 100", c.Cold.Batch)
	}
}

// The location belongs in the startup log; the credentials never do.
func TestRedactedReportsTheColdTierWithoutItsCredentials(t *testing.T) {
	validEnv(t)
	t.Setenv("PC_COLD_TIER_ENABLED", "true")
	t.Setenv("PC_COLD_S3_ENDPOINT", "https://s3.example.invalid")
	t.Setenv("PC_COLD_S3_BUCKET", "cold")
	t.Setenv("PC_COLD_S3_ACCESS_KEY", "key")
	t.Setenv("PC_COLD_S3_SECRET_KEY", "super-secret-value")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	r := c.Redacted()
	if r["cold_tier_enabled"] != true {
		t.Errorf("cold_tier_enabled = %v, want true", r["cold_tier_enabled"])
	}
	if r["cold_tier_bucket"] != "cold" {
		t.Errorf("cold_tier_bucket = %v, want cold", r["cold_tier_bucket"])
	}
	for k, v := range r {
		if s, ok := v.(string); ok && strings.Contains(s, "super-secret-value") {
			t.Fatalf("Redacted() leaked the cold tier secret key through %q", k)
		}
	}
}
