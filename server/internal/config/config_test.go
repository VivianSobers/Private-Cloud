package config

import (
	"strings"
	"testing"
)

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
	t.Setenv("PC_DATABASE_URL", "postgres://u:p@localhost:5432/pc")

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
	t.Setenv("PC_DATABASE_URL", "postgres://u:p@localhost:5432/pc")

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
	t.Setenv("PC_DATABASE_URL", "postgres://u:p@localhost:5432/pc")
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

func TestRedactURLWithoutPassword(t *testing.T) {
	// A URL with no password must pass through unchanged rather than being
	// mangled into something misleading.
	const in = "postgres://user@host:5432/db"
	if got := redactURL(in); got != in {
		t.Errorf("redactURL(%q) = %q, want unchanged", in, got)
	}
}
