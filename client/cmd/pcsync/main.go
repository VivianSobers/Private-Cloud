// Command pcsync is the Private Cloud sync daemon: it keeps one local folder in
// step with the server's tree, in both directions, over the delta protocol.
//
// Run with no subcommand it is the daemon, configured by a JSON file:
//
//	pcsync -config ~/.config/pcsync/config.json        # run as a daemon
//	pcsync -config ~/.config/pcsync/config.json -once  # one reconcile, then exit
//
// The resident daemon also serves a local control socket, which the subcommands
// drive to watch and steer it without touching the config or the logs:
//
//	pcsync status  -config <path>   # is it up to date, syncing, paused, broken?
//	pcsync sync    -config <path>   # reconcile now
//	pcsync pause   -config <path>   # stop automatic syncing
//	pcsync resume  -config <path>   # resume automatic syncing
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
	"time"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/api"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/config"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/control"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/engine"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/state"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// A control subcommand as the first argument talks to a running daemon; anything
	// else is the daemon itself, so the systemd unit's `pcsync -config ...` is
	// unchanged.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "status", "sync", "pause", "resume":
			os.Exit(ctlMain(os.Args[1], os.Args[2:]))
		}
	}

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

	// The state directory holds the local database, download temp files, and the
	// control socket, and is never synced. Create it before opening the database.
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

	// The resident daemon serves the control socket so `pcsync status` and friends
	// can reach it. Its failure is logged, not fatal — the sync loop is the point,
	// and it keeps running even if the control surface could not come up.
	ctl := control.NewServer(eng, control.Info{Server: cfg.ServerURL, Root: cfg.Root, Version: version}, log)
	sock := filepath.Join(cfg.StateDir(), control.SocketName)
	go func() {
		if err := ctl.Serve(sock); err != nil {
			log.Error("control socket failed", "error", err)
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ctl.Shutdown(sctx)
	}()

	if err := eng.Run(ctx, cfg.PollInterval(), cfg.RescanInterval()); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("pcsync stopped")
	return nil
}

// ctlMain runs a control subcommand against the daemon's socket and returns a
// process exit code.
func ctlMain(action string, args []string) int {
	fs := flag.NewFlagSet("pcsync "+action, flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to the JSON config file")
	_ = fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	client := control.NewClient(filepath.Join(cfg.StateDir(), control.SocketName))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	switch action {
	case "status":
		st, err := client.Status(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		printStatus(st)
	case "sync":
		if err := client.Sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("sync requested")
	case "pause":
		if err := client.Pause(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("automatic syncing paused")
	case "resume":
		if err := client.Resume(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("automatic syncing resumed")
	}
	return 0
}

// printStatus renders a status response as a short human-readable block.
func printStatus(st control.StatusResponse) {
	phase := string(st.Phase)
	if st.Paused {
		phase = "paused"
	}
	fmt.Printf("pcsync %s — %s\n", st.Version, st.Server)
	fmt.Printf("  folder:     %s\n", st.Root)
	fmt.Printf("  state:      %s\n", phase)
	fmt.Printf("  tracked:    %d items\n", st.Tracked)
	fmt.Printf("  last sync:  %s\n", humanTime(st.LastSync))
	if st.LastError != "" {
		fmt.Printf("  last error: %s (%s)\n", st.LastError, humanTime(st.LastErrorAt))
	}
	if len(st.Conflicts) > 0 {
		fmt.Printf("  conflicts:  %d need attention\n", len(st.Conflicts))
		for _, c := range st.Conflicts {
			fmt.Printf("    %s  →  %s\n", c.Original, c.Copy)
		}
	}
}

// humanTime renders a timestamp as a coarse "how long ago", or "never" for zero.
func humanTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02 15:04")
	}
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
