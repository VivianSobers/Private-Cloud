package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/config"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/update"
)

// errUpdated is returned by the daemon's run loop once it has replaced its own
// binary on disk. It is not a failure — it is the only way a long-running
// process can start running new code.
var errUpdated = errors.New("pcsync replaced its own binary and must restart")

// exitUpdated is the status the daemon exits with after applying an update.
//
// It is deliberately non-zero, because the shipped systemd unit says
// Restart=on-failure: a clean exit would leave the machine with a new binary on
// disk and nothing running it. A service manager sees a failure and re-execs the
// path — which is now the new build. Anyone supervising pcsync some other way
// only needs to know that 70 means "restart me", and the unit file says so.
const exitUpdated = 70

// newUpdater builds the updater from config. The trust anchors are pinned in the
// binary; the config chooses the feed and the accepted signing identity, and can
// only ever narrow what is accepted, never widen it past the pinned CA.
func newUpdater(cfg *config.Config, currentVersion string) (*update.Updater, error) {
	verifier, err := update.FulcioTrust(cfg.Update.Identity, cfg.Update.Issuer)
	if err != nil {
		return nil, err
	}
	return update.New(update.Options{
		CurrentVersion: currentVersion,
		FeedURL:        cfg.Update.FeedURL,
		Verifier:       verifier,
		AllowDowngrade: cfg.Update.AllowDowngrade,
	})
}

// updateMain is `pcsync update`: check what the feed offers and, with -apply,
// install it. Checking works whether or not the updater is enabled in the config
// — refusing to even *look* would be theatre. Installing does not: -apply on a
// machine that has not opted in is an error naming the setting, so the opt-in
// stays a real decision rather than a flag anyone can route around.
func updateMain(args []string) int {
	fs := flag.NewFlagSet("pcsync update", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to the JSON config file")
	apply := fs.Bool("apply", false, "install the release if one is newer (otherwise only report)")
	_ = fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	u, err := newUpdater(cfg, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rel, err := u.Check(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("running  %s\n", version)
	fmt.Printf("released %s", rel.Feed.Version)
	if !rel.Feed.ReleasedAt.IsZero() {
		fmt.Printf("  (%s)", rel.Feed.ReleasedAt.Format("2006-01-02"))
	}
	fmt.Println()

	switch {
	case !rel.Comparable:
		fmt.Printf("this build's version cannot be compared with a release tag; nothing is assumed\n")
	case !rel.Newer:
		fmt.Println("up to date")
		if !*apply {
			return 0
		}
	default:
		fmt.Printf("a newer release is available: %s\n", rel.Feed.Version)
		if rel.Feed.NotesURL != "" {
			fmt.Printf("notes: %s\n", rel.Feed.NotesURL)
		}
	}
	if !*apply {
		if rel.Newer {
			fmt.Println("run `pcsync update -apply` to install it")
		}
		return 0
	}
	if !cfg.Update.Enabled {
		fmt.Fprintln(os.Stderr, "update: installing is off; set \"update\": {\"enabled\": true} in the config first")
		return 1
	}

	res, err := u.Apply(ctx, rel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("installed %s over %s at %s\n", res.To, res.From, res.Path)
	fmt.Println("restart pcsync to run it (systemctl --user restart pcsync)")
	return 0
}

// autoUpdate is the resident daemon's update cadence. It runs only when the
// config opted in, checks on a slow timer, and returns as soon as it has
// installed something — the caller then unwinds the daemon so the service
// manager can exec the new binary.
//
// Two things it deliberately does not do. It does not retry a failed check
// aggressively: an update is never urgent enough to hammer a release host from
// every laptop at once. And it does not update a packaged install — /usr is
// read-only under the shipped unit's ProtectSystem, so writing to /usr/bin/pcsync
// fails, which is correct: a machine that installed pcsync from apt or dnf should
// update it from apt or dnf, and the failure says so once rather than every hour.
func autoUpdate(ctx context.Context, log *slog.Logger, cfg *config.Config) error {
	u, err := newUpdater(cfg, version)
	if err != nil {
		return err
	}
	interval := cfg.Update.CheckInterval()
	log.Info("automatic updates are on", "every", interval, "feed", cfg.Update.FeedURL)

	// A first check shortly after start, then on the cadence. Not immediately at
	// start: a machine waking up has better things to do for the first minute.
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	var warned bool
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		timer.Reset(interval)

		rel, err := u.Check(ctx)
		if err != nil {
			log.Warn("update check failed", "error", err)
			continue
		}
		if !rel.Comparable || !rel.Newer {
			continue
		}
		log.Info("a newer pcsync is available", "running", version, "release", rel.Feed.Version)
		res, err := u.Apply(ctx, rel)
		if err != nil {
			// Logged once at warn, then at debug: a read-only /usr or a missing
			// permission is a standing condition, not news every hour.
			if warned {
				log.Debug("update not applied", "error", err)
			} else {
				log.Warn("update not applied", "error", err)
				warned = true
			}
			continue
		}
		log.Info("update installed; restarting", "from", res.From, "to", res.To, "path", res.Path)
		return errUpdated
	}
}
