// Package config loads runtime configuration from the environment.
//
// Everything is read once at startup and validated eagerly: a misconfigured
// server should fail immediately and loudly, not three hours later on the first
// request that happens to touch the broken setting.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Env is "dev" or "prod". It only affects defaults and log formatting;
	// never use it to switch security behaviour, or dev becomes the untested
	// configuration that eventually ships.
	Env string

	HTTPAddr        string
	ShutdownTimeout time.Duration

	DatabaseURL      string
	DBMaxConns       int32
	DBMinConns       int32
	DBConnectTimeout time.Duration

	LogLevel  string
	LogFormat string // "json" or "text"

	// --- WebAuthn ---
	//
	// RPID is the "relying party ID": the domain passkeys are bound to. It must
	// be the registrable domain the browser sees, WITHOUT scheme or port
	// ("cloud.tail1234.ts.net"). Getting this wrong is the single most common
	// WebAuthn misconfiguration, and it fails at credential-creation time with
	// an opaque browser error rather than anything useful server-side.
	//
	// Changing RPID later INVALIDATES every existing passkey — they are bound
	// to the origin they were created against. Decide the hostname before
	// enrolling keys you care about.
	WebAuthnRPID    string
	WebAuthnRPName  string
	WebAuthnOrigins []string

	// --- sessions ---
	SessionTTL   time.Duration
	CookieName   string
	CookieSecure bool

	// --- storage ---
	//
	// BlobPath is where file content lives. It must be on the ZFS dataset that
	// Phase 0 set up (tank/data), because that is what sanoid snapshots and
	// restic backs up — a blob store outside it is silently unprotected.
	BlobPath string

	// TrashRetention bounds how long deleted files keep occupying disk.
	TrashRetention time.Duration
	// BlobGCInterval is how often unreferenced blobs are swept.
	BlobGCInterval time.Duration
	// UploadTTL bounds how long an abandoned resumable upload occupies disk.
	// Generous by default: a large file over a slow link legitimately spans
	// days of intermittent connectivity, and expiring it mid-transfer costs
	// the user everything they had already sent.
	UploadTTL time.Duration

	// MigrateOnStart runs pending migrations during startup. Convenient for a
	// single-node deployment; would be wrong with multiple replicas racing to
	// migrate, which is why it is a flag rather than unconditional.
	MigrateOnStart bool

	// BlobMigrateInterval is how often the background pass rewrites Phase 1
	// whole-file blobs into content-addressed chunks. Zero disables it — the
	// default, because a storage rewrite is exactly when a fresh backup should be
	// taken first, so draining the backlog is a deliberate operator act
	// (`cloudctl migrate-blobs`), not something that fires unbidden on deploy.
	BlobMigrateInterval time.Duration
	// BlobMigrateBatch bounds how many versions one pass rewrites, so the drain
	// stays incremental and a single tick cannot monopolise disk and CPU on a
	// small box for an unbounded time.
	BlobMigrateBatch int
}

func Load() (*Config, error) {
	c := &Config{
		Env:             env("PC_ENV", "dev"),
		HTTPAddr:        env("PC_HTTP_ADDR", ":8080"),
		ShutdownTimeout: envDuration("PC_SHUTDOWN_TIMEOUT", 20*time.Second),

		DatabaseURL:      env("PC_DATABASE_URL", ""),
		DBMaxConns:       int32(envInt("PC_DB_MAX_CONNS", 10)),
		DBMinConns:       int32(envInt("PC_DB_MIN_CONNS", 2)),
		DBConnectTimeout: envDuration("PC_DB_CONNECT_TIMEOUT", 10*time.Second),

		LogLevel:  env("PC_LOG_LEVEL", "info"),
		LogFormat: env("PC_LOG_FORMAT", "json"),

		WebAuthnRPID:    env("PC_WEBAUTHN_RPID", "localhost"),
		WebAuthnRPName:  env("PC_WEBAUTHN_RP_NAME", "Private Cloud"),
		WebAuthnOrigins: envList("PC_WEBAUTHN_ORIGINS", []string{"http://localhost:8080"}),

		SessionTTL: envDuration("PC_SESSION_TTL", 30*24*time.Hour),
		CookieName: env("PC_COOKIE_NAME", "pc_session"),
		// Secure cookies by default; only a dev run over plain HTTP should
		// ever turn this off, and it must be an explicit act.
		CookieSecure: envBool("PC_COOKIE_SECURE", true),

		BlobPath:       env("PC_BLOB_PATH", "/data/blobs"),
		TrashRetention: envDuration("PC_TRASH_RETENTION", 30*24*time.Hour),
		BlobGCInterval: envDuration("PC_BLOB_GC_INTERVAL", 6*time.Hour),
		UploadTTL:      envDuration("PC_UPLOAD_TTL", 48*time.Hour),

		MigrateOnStart: envBool("PC_MIGRATE_ON_START", true),

		// Off by default: the operator drains Phase 1 blobs deliberately, after a
		// backup, rather than having a deploy start rewriting storage on its own.
		BlobMigrateInterval: envDuration("PC_BLOB_MIGRATE_INTERVAL", 0),
		BlobMigrateBatch:    envInt("PC_BLOB_MIGRATE_BATCH", 100),
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("PC_DATABASE_URL is required")
	}
	// Catch the copy-paste mistake where the URL is a bare hostname.
	if !strings.HasPrefix(c.DatabaseURL, "postgres://") &&
		!strings.HasPrefix(c.DatabaseURL, "postgresql://") {
		return fmt.Errorf("PC_DATABASE_URL must be a postgres:// URL")
	}
	switch c.Env {
	case "dev", "prod":
	default:
		return fmt.Errorf("PC_ENV must be dev or prod, got %q", c.Env)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("PC_LOG_FORMAT must be json or text, got %q", c.LogFormat)
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("PC_LOG_LEVEL must be debug/info/warn/error, got %q", c.LogLevel)
	}
	if c.DBMinConns > c.DBMaxConns {
		return fmt.Errorf("PC_DB_MIN_CONNS (%d) exceeds PC_DB_MAX_CONNS (%d)", c.DBMinConns, c.DBMaxConns)
	}

	// A scheme or port in RPID is the classic WebAuthn misconfiguration, and
	// the browser-side failure it produces is famously unhelpful. Reject it
	// here, where the message can actually say what is wrong.
	if strings.Contains(c.WebAuthnRPID, "://") || strings.Contains(c.WebAuthnRPID, ":") ||
		strings.Contains(c.WebAuthnRPID, "/") {
		return fmt.Errorf("PC_WEBAUTHN_RPID must be a bare domain with no scheme, port or path (got %q)", c.WebAuthnRPID)
	}
	if len(c.WebAuthnOrigins) == 0 {
		return fmt.Errorf("PC_WEBAUTHN_ORIGINS must list at least one origin")
	}
	// Origins are the mirror image: they MUST carry a scheme.
	for _, o := range c.WebAuthnOrigins {
		if !strings.HasPrefix(o, "http://") && !strings.HasPrefix(o, "https://") {
			return fmt.Errorf("PC_WEBAUTHN_ORIGINS entries must include a scheme (got %q)", o)
		}
	}

	// Refuse the combination that silently ships session cookies in the clear.
	if c.Env == "prod" && !c.CookieSecure {
		return fmt.Errorf("PC_COOKIE_SECURE=false is not allowed when PC_ENV=prod")
	}

	if c.BlobPath == "" {
		return fmt.Errorf("PC_BLOB_PATH is required")
	}
	// A relative path resolves against whatever the working directory happens
	// to be, which differs between `make run`, the container and systemd. For
	// the one directory holding every byte the user owns, that is not a risk
	// worth taking.
	if !filepath.IsAbs(c.BlobPath) {
		return fmt.Errorf("PC_BLOB_PATH must be an absolute path (got %q)", c.BlobPath)
	}
	// A retention of zero would purge the trash on its first sweep, which is
	// indistinguishable from having no trash at all.
	if c.TrashRetention <= 0 {
		return fmt.Errorf("PC_TRASH_RETENTION must be positive")
	}
	if c.BlobGCInterval <= 0 {
		return fmt.Errorf("PC_BLOB_GC_INTERVAL must be positive")
	}
	if c.UploadTTL <= 0 {
		return fmt.Errorf("PC_UPLOAD_TTL must be positive")
	}
	// The interval is allowed to be zero — that is how the background drain is
	// disabled — but an enabled loop with a non-positive batch would list zero
	// candidates every tick and never make progress, a silent no-op worse than
	// an error.
	if c.BlobMigrateInterval > 0 && c.BlobMigrateBatch <= 0 {
		return fmt.Errorf("PC_BLOB_MIGRATE_BATCH must be positive when PC_BLOB_MIGRATE_INTERVAL is set")
	}
	return nil
}

// Redacted returns the config with the database password masked, for logging.
// Logging a config struct verbatim is how credentials end up in Loki.
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		"env":              c.Env,
		"http_addr":        c.HTTPAddr,
		"database_url":     redactURL(c.DatabaseURL),
		"db_max_conns":     c.DBMaxConns,
		"log_level":        c.LogLevel,
		"log_format":       c.LogFormat,
		"blob_path":             c.BlobPath,
		"trash_retention":       c.TrashRetention.String(),
		"migrate_on_start":      c.MigrateOnStart,
		"blob_migrate_interval": c.BlobMigrateInterval.String(),
		"blob_migrate_batch":    c.BlobMigrateBatch,
	}
}

// redactURL masks the password in a postgres URL, preserving enough of the
// rest to be useful when debugging "why can't it connect".
func redactURL(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	scheme := strings.Index(raw, "://")
	if scheme < 0 {
		return "***"
	}
	creds := raw[scheme+3 : at]
	colon := strings.Index(creds, ":")
	if colon < 0 {
		return raw // no password present
	}
	return raw[:scheme+3] + creds[:colon] + ":***" + raw[at:]
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// envList parses a comma-separated list, trimming whitespace and dropping
// empties so a trailing comma doesn't become an empty origin.
func envList(key string, def []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
