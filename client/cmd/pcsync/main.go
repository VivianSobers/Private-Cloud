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
	"strings"
	"syscall"
	"time"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/api"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/config"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/control"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/engine"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/state"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/tray"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// A control subcommand as the first argument talks to a running daemon; anything
	// else is the daemon itself, so the systemd unit's `pcsync -config ...` is
	// unchanged.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "status", "watch", "sync", "pause", "resume", "exclude", "conflicts":
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
	// Seed selective-sync from the config only on first run; a live change made
	// through the control surface persists and wins thereafter.
	eng.SeedExcludes(cfg.Excludes)

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

	// `exclude` takes up to two leading positionals (a sub-action and a path) before
	// its flags, so `pcsync exclude add /Videos -config c.json` parses cleanly.
	var sub, exPath string
	if action == "exclude" || action == "conflicts" {
		args, sub, exPath = leadingPositionals(args)
	}
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
	case "watch":
		return ctlWatch(client)
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
	case "exclude":
		return ctlExclude(ctx, client, sub, exPath)
	case "conflicts":
		return ctlConflicts(ctx, client, sub)
	}
	return 0
}

// ctlConflicts lists the conflict copies awaiting a decision, or clears the log
// once they have been dealt with.
func ctlConflicts(ctx context.Context, client *control.Client, sub string) int {
	switch sub {
	case "", "list":
		conflicts, err := client.Conflicts(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if len(conflicts) == 0 {
			fmt.Println("no conflicts — nothing needs your attention")
			return 0
		}
		fmt.Printf("%d conflict(s) — the server's version kept the original name; your\n", len(conflicts))
		fmt.Println("edit was set aside as the copy. Keep whichever you want, delete the other:")
		for _, c := range conflicts {
			fmt.Printf("  %s\n    server → %s\n    yours  → %s\n", tray.RelTime(c.At), c.Original, c.Copy)
		}
	case "clear":
		if err := client.ClearConflicts(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("conflict list cleared")
	default:
		fmt.Fprintf(os.Stderr, "unknown conflicts action %q (want list, clear)\n", sub)
		return 2
	}
	return 0
}

// leadingPositionals peels up to two leading non-flag arguments off args, so a
// subcommand can take positionals before its flags.
func leadingPositionals(args []string) (rest []string, first, second string) {
	rest = args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		first, rest = rest[0], rest[1:]
	}
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		second, rest = rest[0], rest[1:]
	}
	return rest, first, second
}

// ctlExclude runs the selective-sync subcommands: list, add <path>, remove <path>.
func ctlExclude(ctx context.Context, client *control.Client, sub, path string) int {
	switch sub {
	case "", "list":
		ex, err := client.Excludes(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		printExcludes(ex)
	case "add", "remove":
		if path == "" {
			fmt.Fprintf(os.Stderr, "usage: pcsync exclude %s <server-path> -config <file>\n", sub)
			return 2
		}
		current, err := client.Excludes(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		next := editExcludes(current, sub, path)
		updated, err := client.SetExcludes(ctx, next)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		printExcludes(updated)
	default:
		fmt.Fprintf(os.Stderr, "unknown exclude action %q (want list, add, remove)\n", sub)
		return 2
	}
	return 0
}

// editExcludes applies an add or remove to a set, matching a removal against the
// normalized form so `Videos`, `/Videos` and `/Videos/` all remove the same rule.
func editExcludes(current []string, action, path string) []string {
	if action == "add" {
		return append(current, path)
	}
	target := "/" + strings.Trim(strings.TrimSpace(path), "/")
	var out []string
	for _, p := range current {
		if p != target {
			out = append(out, p)
		}
	}
	return out
}

// printExcludes renders the selective-sync set.
func printExcludes(ex []string) {
	if len(ex) == 0 {
		fmt.Println("no folders excluded — syncing the whole tree")
		return
	}
	fmt.Println("excluded from this device:")
	for _, p := range ex {
		fmt.Printf("  %s\n", p)
	}
}

// ctlWatch renders a live, self-updating status line until interrupted — the
// headless counterpart to a tray icon, over the same control socket.
func ctlWatch(client *control.Client) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	render := func() {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		st, err := client.Status(rctx)
		reachable := err == nil
		// \r returns to the line start and \033[K clears to end of line, so the
		// summary updates in place rather than scrolling.
		fmt.Printf("\r\033[K%s  %s", tray.Derive(st, reachable).Glyph(), tray.Summary(st, reachable))
	}

	render()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			return 0
		case <-ticker.C:
			render()
		}
	}
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
	if st.IgnoreRules > 0 {
		fmt.Printf("  ignoring:   %d .pcsyncignore rule(s)\n", st.IgnoreRules)
	}
	fmt.Printf("  last sync:  %s\n", tray.RelTime(st.LastSync))
	fmt.Printf("  transfers:  ↓ %d files (%s) · ↑ %d files (%s)  this session\n",
		st.PulledFiles, tray.HumanBytes(st.PulledBytes), st.PushedFiles, tray.HumanBytes(st.PushedBytes))
	if st.LastError != "" {
		fmt.Printf("  last error: %s (%s)\n", st.LastError, tray.RelTime(st.LastErrorAt))
	}
	if len(st.Conflicts) > 0 {
		fmt.Printf("  conflicts:  %d need attention\n", len(st.Conflicts))
		for _, c := range st.Conflicts {
			fmt.Printf("    %s  →  %s\n", c.Original, c.Copy)
		}
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
