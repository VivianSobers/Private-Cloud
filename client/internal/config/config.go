// Package config loads the sync daemon's settings from a JSON file. The daemon
// is headless and configured by a file, not flags — one synced root, one server,
// one credential — so the whole configuration is small enough to read at a glance
// and version alongside the machine it runs on.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the on-disk shape.
type Config struct {
	// ServerURL is the base URL of the API, e.g. https://cloud.example.ts.net.
	ServerURL string `json:"server_url"`
	// Username identifies the account; it is checked against the app password's
	// account so a mismatched pair fails loudly rather than syncing the wrong tree.
	Username string `json:"username"`
	// AppPassword is a pcap_ credential minted with `cloudctl` or the web UI. It
	// is exchanged for a device token; it is never sent to the file endpoints.
	AppPassword string `json:"app_password"`
	// Root is the local folder that mirrors the server tree.
	Root string `json:"root"`
	// StateDB is where the local sync database lives. Defaults to
	// <root>/.pcsync/state.db, kept out of the synced tree itself.
	StateDB string `json:"state_db"`
	// PollSeconds is how often the change journal is polled. fsnotify makes local
	// pushes responsive between polls; this bounds how stale a remote change can be.
	PollSeconds int `json:"poll_seconds"`
	// RescanSeconds is how often a full local rescan runs, catching anything the
	// filesystem watcher missed. A missed inotify event is caught here, not lost.
	RescanSeconds int `json:"rescan_seconds"`
}

// Defaults applied when a field is left zero.
const (
	defaultPollSeconds   = 15
	defaultRescanSeconds = 300
	stateDirName         = ".pcsync"
)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields() // a typo'd key is a misconfiguration, not something to ignore silently
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) normalize() error {
	c.ServerURL = strings.TrimRight(strings.TrimSpace(c.ServerURL), "/")
	c.Username = strings.TrimSpace(c.Username)
	c.AppPassword = strings.TrimSpace(c.AppPassword)

	switch {
	case c.ServerURL == "":
		return fmt.Errorf("config: server_url is required")
	case !strings.HasPrefix(c.ServerURL, "http://") && !strings.HasPrefix(c.ServerURL, "https://"):
		return fmt.Errorf("config: server_url must be an http(s) URL")
	case c.AppPassword == "":
		return fmt.Errorf("config: app_password is required")
	case c.Root == "":
		return fmt.Errorf("config: root is required")
	}

	abs, err := filepath.Abs(c.Root)
	if err != nil {
		return fmt.Errorf("config: resolve root: %w", err)
	}
	c.Root = abs

	if c.StateDB == "" {
		c.StateDB = filepath.Join(c.Root, stateDirName, "state.db")
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = defaultPollSeconds
	}
	if c.RescanSeconds <= 0 {
		c.RescanSeconds = defaultRescanSeconds
	}
	return nil
}

// PollInterval and RescanInterval express the tick settings as durations.
func (c *Config) PollInterval() time.Duration   { return time.Duration(c.PollSeconds) * time.Second }
func (c *Config) RescanInterval() time.Duration { return time.Duration(c.RescanSeconds) * time.Second }

// StateDir is the directory holding the state database, which the daemon creates
// and excludes from syncing.
func (c *Config) StateDir() string { return filepath.Dir(c.StateDB) }
