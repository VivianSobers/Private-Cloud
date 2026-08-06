// Command pcsync is the Private Cloud sync daemon: it keeps one local folder in
// step with the server's tree, in both directions, over the delta protocol.
//
// It is headless and configured by a JSON file — no flags beyond where that file
// is and whether to run once or stay resident:
//
//	pcsync -config ~/.config/pcsync/config.json        # run as a daemon
//	pcsync -config ~/.config/pcsync/config.json -once  # one reconcile, then exit
//
// The one-shot mode is what a cron job or a manual "sync now" uses; the resident
// mode watches the filesystem and polls the change journal.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/api"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/config"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/engine"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/state"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON config file")
	once := flag.Bool("once", false, "run a single reconcile and exit, instead of staying resident")
	verbose := flag.Bool("v", false, "log at debug level")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(log, *configPath, *once); err != nil {
		log.Error("pcsync exiting", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, configPath string, once bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// The state directory holds the local database and download temp files, and is
	// never synced. Create it before opening the database inside it.
	if err := os.MkdirAll(cfg.StateDir(), 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	st, err := state.Open(cfg.StateDB)
	if err != nil {
		return err
	}
	defer st.Close()

	client := api.New(cfg.ServerURL, cfg.Username, cfg.AppPassword, userAgent())
	eng := engine.New(client, st, cfg.Root, cfg.StateDir(), log)

	// SIGINT/SIGTERM cancel the context, so a reconcile in flight finishes its
	// current step and the daemon exits cleanly rather than mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("pcsync starting", "version", version, "root", cfg.Root, "server", cfg.ServerURL)

	if once {
		return eng.Sync(ctx)
	}
	if err := eng.Run(ctx, cfg.PollInterval(), cfg.RescanInterval()); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("pcsync stopped")
	return nil
}

// userAgent identifies this client to the server, so a device session in the
// account's session list is recognizable as "which machine".
func userAgent() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("pcsync/%s (%s)", version, filepath.Base(host))
}
