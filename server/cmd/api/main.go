// Command api is the private-cloud HTTP server.
//
// Phase 1, slice 1: skeleton only — configuration, database, migrations,
// health probes and metrics. File and auth endpoints arrive in slices 2 and 3.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/config"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/httpapi"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/metrics"
)

// Injected at build time via -ldflags; see the Dockerfile and Makefile.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	// The container image is distroless: no shell, no wget, nothing that could
	// run a healthcheck command. So the binary checks itself, and compose
	// invokes `/api healthcheck`.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	// run() returns an error instead of calling os.Exit inline so that every
	// deferred cleanup actually runs. os.Exit skips defers.
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// healthcheck probes the local liveness endpoint and maps it to an exit code.
//
// It deliberately hits /healthz rather than /readyz: Docker restarts a
// container whose healthcheck fails, and restarting the API because Postgres
// is briefly down would convert a database blip into an application crash loop.
func healthcheck() int {
	addr := os.Getenv("PC_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// ":8080" -> "127.0.0.1:8080"; an addr already carrying a host is used as-is.
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	log := newLogger(cfg)
	slog.SetDefault(log)
	log.Info("starting private-cloud api", "version", version, "commit", commit)
	log.Info("configuration loaded", "config", cfg.Redacted())

	// NotifyContext cancels on SIGINT/SIGTERM. Everything below takes this
	// context, so a shutdown signal during startup aborts cleanly rather than
	// being ignored until the server is already listening.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBMinConns, cfg.DBConnectTimeout, log)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer database.Close()

	if cfg.MigrateOnStart {
		if err := database.Migrate(ctx, log); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	} else {
		log.Warn("migrations skipped (PC_MIGRATE_ON_START=false)")
	}

	m := metrics.New(version, commit, func() float64 {
		return float64(database.Pool.Stat().AcquiredConns())
	})

	if v, err := database.SchemaVersion(ctx); err == nil {
		m.SchemaVersion.Set(float64(v))
	} else {
		log.Warn("could not read schema version for metrics", "error", err)
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpapi.NewServer(log, database, m, version, commit).Handler(),

		// Timeouts are not optional on a server that will accept uploads from
		// the internet-adjacent world. Without them a handful of slow-loris
		// connections exhaust the listener.
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout is deliberately absent: slice 4 adds large resumable
		// uploads, and a global read timeout would kill legitimate slow
		// transfers. Per-route deadlines via http.ResponseController are the
		// right tool there. ReadHeaderTimeout still covers slow-loris.
		WriteTimeout: 0, // same reasoning, for downloads
		IdleTimeout:  120 * time.Second,

		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// Serve in a goroutine so main can wait on either a signal or a listen
	// failure, whichever happens first.
	serveErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Info("shutdown signal received", "timeout", cfg.ShutdownTimeout)
	}

	// Stop listening and let in-flight requests finish. context.Background is
	// correct here: ctx is already cancelled, and passing it would abort the
	// drain immediately — defeating the point of graceful shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}

	log.Info("shutdown complete")
	return nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	// JSON in production so Loki can parse it without regex; text in dev
	// because reading JSON by eye is miserable.
	var h slog.Handler
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}
