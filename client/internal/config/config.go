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
	"regexp"
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
	// Excludes is the selective-sync set: server-path prefixes this device does not
	// sync (e.g. "/Videos"). Excluded subtrees are never downloaded here and their
	// absence never deletes them on the server. Empty means sync the whole tree.
	Excludes []string `json:"excludes"`
	// Update configures the in-place updater. It is OFF unless "update".enabled
	// is set: a program that can replace its own binary is a program that can be
	// made to run somebody else's, so the capability is opt-in per machine rather
	// than something an install quietly turns on.
	Update UpdateConfig `json:"update"`
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
	if err := c.Update.normalize(); err != nil {
		return err
	}
	return nil
}

// PollInterval and RescanInterval express the tick settings as durations.
func (c *Config) PollInterval() time.Duration   { return time.Duration(c.PollSeconds) * time.Second }
func (c *Config) RescanInterval() time.Duration { return time.Duration(c.RescanSeconds) * time.Second }

// StateDir is the directory holding the state database, which the daemon creates
// and excludes from syncing.
func (c *Config) StateDir() string { return filepath.Dir(c.StateDB) }

// UpdateConfig is the in-place updater's settings. The zero value is "off",
// which is the shipped default and the only default that can be justified: an
// updater is a remote code-execution channel you have pointed at yourself, and
// it should exist because somebody chose it, not because it came switched on.
//
// Everything below the switch has a working default, so opting in is one line:
//
//	"update": { "enabled": true }
type UpdateConfig struct {
	// Enabled turns the updater on. Off means pcsync never fetches the feed and
	// never writes to its own binary.
	Enabled bool `json:"enabled"`
	// FeedURL is the JSON release feed. It must be https: an updater reading a
	// plaintext feed is an install-anything button for anyone on the path.
	FeedURL string `json:"feed_url"`
	// CheckHours is how often the resident daemon looks for a new release.
	CheckHours int `json:"check_hours"`
	// Identity is the cosign certificate identity (SAN) the release signature
	// must carry, as a regular expression. It pins updates to signatures minted
	// by this project's release workflow — without it, "signed by somebody with
	// a GitHub account" would be the whole of the check.
	Identity string `json:"identity"`
	// Issuer is the OIDC issuer that minted that identity.
	Issuer string `json:"issuer"`
	// AllowDowngrade permits installing an older version than the running one.
	// Off by default, because a feed rolled back to an old, known-bad release is
	// exactly how a signed-artifact channel gets turned against its users.
	AllowDowngrade bool `json:"allow_downgrade"`
}

// Updater defaults. The identity is a regular expression matched against the
// certificate's SAN, so it pins both the repository and the workflow file that
// is allowed to sign a release — a different workflow in the same repo does not
// inherit the trust.
const (
	DefaultFeedURL    = "https://github.com/guru-bharadwaj20/private-cloud/releases/latest/download/update-feed.json"
	DefaultIdentity   = `^https://github\.com/guru-bharadwaj20/private-cloud/\.github/workflows/release\.yml@refs/tags/`
	DefaultIssuer     = "https://token.actions.githubusercontent.com"
	defaultCheckHours = 24
)

// normalize fills the updater's defaults and rejects the settings that would
// make the check meaningless. It only validates when the updater is enabled —
// a disabled updater with a half-filled block is not a misconfiguration.
func (u *UpdateConfig) normalize() error {
	u.FeedURL = strings.TrimSpace(u.FeedURL)
	u.Identity = strings.TrimSpace(u.Identity)
	u.Issuer = strings.TrimSpace(u.Issuer)

	if u.FeedURL == "" {
		u.FeedURL = DefaultFeedURL
	}
	if u.Identity == "" {
		u.Identity = DefaultIdentity
	}
	if u.Issuer == "" {
		u.Issuer = DefaultIssuer
	}
	if u.CheckHours <= 0 {
		u.CheckHours = defaultCheckHours
	}
	if !u.Enabled {
		return nil
	}
	if !strings.HasPrefix(u.FeedURL, "https://") {
		return fmt.Errorf("config: update.feed_url must be an https URL")
	}
	if _, err := regexp.Compile(u.Identity); err != nil {
		return fmt.Errorf("config: update.identity is not a valid regular expression: %w", err)
	}
	return nil
}

// CheckInterval expresses the update check cadence as a duration.
func (u *UpdateConfig) CheckInterval() time.Duration {
	return time.Duration(u.CheckHours) * time.Hour
}
