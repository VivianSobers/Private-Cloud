package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAppliesDefaults(t *testing.T) {
	// The root is taken from t.TempDir rather than a hardcoded "/tmp/sync",
	// because Load absolutises it (filepath.Abs) and an absolute path is
	// spelled differently per platform: on Windows a rooted "/tmp/sync"
	// acquires the current drive letter, so the expectation and the value
	// agreed only on POSIX. This client cross-compiles to Windows and macOS,
	// so a test that can only pass on one of them is testing the developer's
	// machine rather than the code.
	root := t.TempDir()
	p := write(t, `{
		"server_url": "https://cloud.example.ts.net/",
		"username": "vivian",
		"app_password": "pcap_0011223344556677_00112233445566778899aabbccddeeff",
		"root": `+strconv.Quote(root)+`
	}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ServerURL != "https://cloud.example.ts.net" {
		t.Errorf("trailing slash not trimmed: %q", c.ServerURL)
	}
	if c.PollSeconds != defaultPollSeconds || c.RescanSeconds != defaultRescanSeconds {
		t.Errorf("defaults not applied: poll=%d rescan=%d", c.PollSeconds, c.RescanSeconds)
	}
	if want := filepath.Join(root, stateDirName, "state.db"); c.StateDB != want {
		t.Errorf("state db default wrong: %q, want %q", c.StateDB, want)
	}
	if want := filepath.Join(root, stateDirName); c.StateDir() != want {
		t.Errorf("state dir wrong: %q, want %q", c.StateDir(), want)
	}
	if !filepath.IsAbs(c.Root) {
		t.Errorf("root not absolutised: %q", c.Root)
	}
}

func TestLoadRejectsMissingFields(t *testing.T) {
	cases := map[string]string{
		"no server":   `{"app_password":"x","root":"/tmp/s"}`,
		"bad scheme":  `{"server_url":"cloud.example","app_password":"x","root":"/tmp/s"}`,
		"no password": `{"server_url":"https://c","root":"/tmp/s"}`,
		"no root":     `{"server_url":"https://c","app_password":"x"}`,
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	// A typo'd key is a misconfiguration the user wants told about, not silently
	// dropped — a "sync_root" that never takes effect is a support nightmare.
	p := write(t, `{"server_url":"https://c","app_password":"x","root":"/tmp/s","sync_root":"/tmp/s"}`)
	if _, err := Load(p); err == nil {
		t.Error("unknown key was accepted")
	}
}
