// Package config loads runtime configuration from the environment.
//
// Everything is read once at startup and validated eagerly: a misconfigured
// server should fail immediately and loudly, not three hours later on the first
// request that happens to touch the broken setting.
package config

import (
	"fmt"
	"os"
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

	// MigrateOnStart runs pending migrations during startup. Convenient for a
	// single-node deployment; would be wrong with multiple replicas racing to
	// migrate, which is why it is a flag rather than unconditional.
	MigrateOnStart bool
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

		MigrateOnStart: envBool("PC_MIGRATE_ON_START", true),
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
		"migrate_on_start": c.MigrateOnStart,
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
