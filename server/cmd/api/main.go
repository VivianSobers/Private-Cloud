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

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/config"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
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

	authSvc, err := auth.NewService(
		auth.NewStore(database.Pool),
		auth.Config{
			RPDisplayName: cfg.WebAuthnRPName,
			RPID:          cfg.WebAuthnRPID,
			RPOrigins:     cfg.WebAuthnOrigins,
			SessionTTL:    cfg.SessionTTL,
		},
		log,
	)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	log.Info("webauthn configured", "rpid", cfg.WebAuthnRPID, "origins", cfg.WebAuthnOrigins)

	// Constructed before the listener starts: an unwritable blob directory must
	// stop the process here, not surface as a 500 on the first upload.
	blobs, err := blob.NewFSStore(cfg.BlobPath)
	if err != nil {
		return fmt.Errorf("blob store: %w", err)
	}
	log.Info("blob store ready", "path", blobs.Root())

	filesSvc := files.NewService(files.NewStore(database.Pool), blobs, log)
	filesSvc.TrashRetention = cfg.TrashRetention
	filesSvc.UploadTTL = cfg.UploadTTL
	filesSvc.KeepVersions = cfg.KeepVersions
	filesSvc.VersionRetention = cfg.VersionRetention

	// Content-addressed storage. Uploads do not route through it yet, but fsck
	// and GC must know the format exists BEFORE anything writes it — a checker
	// that only understands whole-file blobs would classify every chunk on disk
	// as an orphan and delete it.
	casStore, err := cas.NewStore(database.Pool, blobs)
	if err != nil {
		return fmt.Errorf("content-addressed store: %w", err)
	}
	filesSvc.SetCAS(casStore)

	// Expired sessions and abandoned ceremonies would otherwise accumulate
	// forever. Cheap enough to run in-process rather than adding a job queue
	// for a single periodic DELETE.
	go runCleanup(ctx, authSvc, log)
	go runGC(ctx, filesSvc, m, cfg.BlobGCInterval, log)

	// Off unless an interval is configured: draining Phase 1 blobs is a rewrite
	// the operator schedules after a backup, not a deploy-time surprise.
	if cfg.BlobMigrateInterval > 0 {
		go runMigration(ctx, filesSvc, m, cfg.BlobMigrateInterval, cfg.BlobMigrateBatch, log)
		log.Info("background blob migration enabled",
			"interval", cfg.BlobMigrateInterval.String(), "batch", cfg.BlobMigrateBatch)
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewServer(log, database, m, authSvc, filesSvc, httpapi.Options{
			Version:      version,
			Commit:       commit,
			CookieName:   cfg.CookieName,
			CookieSecure: cfg.CookieSecure,
		}).Handler(),

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

// runCleanup periodically sweeps expired sessions and ceremonies until the
// process shuts down.
func runCleanup(ctx context.Context, svc *auth.Service, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sessions, ceremonies, err := svc.Store().Cleanup(ctx)
			if err != nil {
				log.Warn("auth cleanup failed", "error", err)
				continue
			}
			if sessions > 0 || ceremonies > 0 {
				log.Info("auth cleanup", "sessions", sessions, "ceremonies", ceremonies)
			}
		}
	}
}

// runGC reclaims expired trash and unreferenced blobs.
//
// Deliberately not run at startup: a restart loop would otherwise turn into a
// tight deletion loop, and there is nothing urgent about reclaiming space that
// has already been unreferenced for hours.
func runGC(ctx context.Context, svc *files.Service, m *metrics.Metrics, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			res, err := svc.CollectGarbage(ctx)
			if err != nil {
				log.Warn("garbage collection failed", "error", err)
				continue
			}
			m.GCBlobsFreed.Add(float64(res.BlobsFreed))
			m.GCBytesFreed.Add(float64(res.BytesFreed))
			if res.TrashPurged > 0 || res.BlobsFreed > 0 || res.UploadsExpired > 0 ||
				res.StagingFreed > 0 || res.ChunksFreed > 0 {
				log.Info("garbage collected",
					"trash_purged", res.TrashPurged,
					"blobs_freed", res.BlobsFreed,
					"bytes_freed", res.BytesFreed,
					"uploads_expired", res.UploadsExpired,
					"staging_freed", res.StagingFreed,
					"chunks_freed", res.ChunksFreed,
					"chunk_bytes_freed", res.ChunkBytesFreed)
			}
		}
	}
}

// runMigration drains Phase 1 whole-file blobs into content-addressed chunks, a
// bounded batch per tick.
//
// Opt-in (interval zero disables it) and never at startup, for the same reason
// as GC: a restart loop must not become a tight rewrite loop, and there is
// nothing urgent about re-encoding bytes that have sat as whole files for
// months. When the backlog is drained a pass finds nothing and costs one query.
func runMigration(ctx context.Context, svc *files.Service, m *metrics.Metrics, interval time.Duration, batch int, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			res, err := svc.MigrateBlobs(ctx, batch)
			if err != nil {
				log.Warn("blob migration failed", "error", err)
				continue
			}
			m.MigratedVersions.Add(float64(res.VersionsMigrated))
			m.MigratedBytes.Add(float64(res.BytesWritten))
			if res.VersionsMigrated > 0 || res.Failed > 0 {
				log.Info("blobs migrated",
					"versions", res.VersionsMigrated,
					"bytes_written", res.BytesWritten,
					"deduped", res.Deduped,
					"skipped", res.Skipped,
					"failed", res.Failed)
			}
		}
	}
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
