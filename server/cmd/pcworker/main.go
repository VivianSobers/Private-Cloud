// Command pcworker drains the background job queue: OCR, embeddings, tagging.
//
// It is a SEPARATE process from the API on purpose. The always-on box is 7 GiB
// and one spinner, and the intelligence features load models the API must never
// hold resident. Running them here means the file and sync API keeps its memory
// budget, and stopping this process degrades the system to exactly the Phase 3
// system — every file endpoint still works, search still works on filenames.
//
// It connects, reaps jobs abandoned by a crashed worker, prunes finished ones,
// and dispatches to whatever handlers are registered. Which handlers those are
// depends on what this box can reach, which is what makes the two-tier
// deployment work:
//
//   - extract (text, PDF, OCR, tagging) registers when PC_BLOB_PATH points at the
//     blob store, so it runs where the bytes are.
//   - embed registers when PC_EMBED_URL points at an inference sidecar, needing
//     only the database and that sidecar — so it can run on a GPU box that cannot
//     see the blob store at all.
//
// A worker with neither configured idles harmlessly.
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

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/config"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/metrics"
)

var (
	version = "dev"
	commit  = "none"
)

func main() {
	if err := run(); err != nil {
		slog.Error("pcworker fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := newLogger()
	slog.SetDefault(log)
	log.Info("starting pcworker", "version", version, "commit", commit)

	// The worker reads only what it needs rather than the API's full config, so it
	// stays independent of the WebAuthn origins and cookie settings it has no use
	// for. It shares the config package's env helpers, though, so an unparseable
	// value is reported here exactly as it would be in the API.
	dsn := os.Getenv("PC_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("PC_DATABASE_URL is required")
	}

	// Validated up front, like the API's config: a sidecar URL with a typo or a
	// zero dimension must stop the worker here, not surface hours later as
	// embed jobs that dead-letter or as vectors nothing can ever match.
	embedCfg, err := config.LoadEmbed()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, dsn, 4, 1, 10*time.Second, log)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer database.Close()

	store := jobs.NewStore(database.Pool)
	runner := jobs.NewRunner(store, log, jobs.Options{
		Idle:  config.EnvDuration("PC_WORKER_IDLE", 2*time.Second),
		Lease: config.EnvDuration("PC_WORKER_LEASE", 10*time.Minute),
	})

	// The handlers need to read file content. Co-located here (same box as the blob
	// store), they read blobs directly; PC_BLOB_PATH is required for that. A GPU
	// worker on another box would instead pull content over the download API —
	// same handlers, different Opener — but that adapter is not wired yet.
	if err := registerHandlers(ctx, runner, store, database, embedCfg, log); err != nil {
		return err
	}

	// The worker's own metrics endpoint. Job counts by state come from the API,
	// which polls the queue table — but only this process knows what its handlers
	// actually DID: how long an OCR took, how often embedding fails, whether a
	// handler is panicking. Serving them here is also what gives the container a
	// real healthcheck, which it previously had no way to offer.
	m := metrics.New(version, commit, func() float64 {
		return float64(database.Pool.Stat().AcquiredConns())
	})
	runner.Observe(func(kind, outcome string, d time.Duration) {
		m.JobsProcessed.WithLabelValues(kind, outcome).Inc()
		m.JobDuration.WithLabelValues(kind).Observe(d.Seconds())
	})
	if addr := os.Getenv("PC_WORKER_METRICS_ADDR"); addr != "" {
		go serveMetrics(ctx, addr, m, log)
	}

	retention := config.EnvDuration("PC_JOB_RETENTION", 7*24*time.Hour)
	queuedTTL := config.EnvDuration("PC_JOB_QUEUED_TTL", 30*24*time.Hour)
	// Every duration above has now been read, so report any that failed to parse
	// rather than running on a default the operator did not choose.
	if err := config.TakeParseErrors(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	go prune(ctx, store, retention, queuedTTL, log)

	log.Info("pcworker draining queue")
	if err := runner.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("pcworker stopped")
	return nil
}

// registerHandlers wires the worker's handlers by what this box can reach — the
// heart of the two-tier design:
//
//   - EMBEDDING needs only the database and the sidecar, never blob content, so it
//     registers whenever PC_EMBED_URL is set. This is what lets a GPU box (which
//     cannot see the blob store) run an embedding-only worker.
//   - EXTRACTION (OCR, text, tagging) needs file bytes, so it registers only when
//     PC_BLOB_PATH points at the blobs — a co-located worker on the always-on box.
//   - The extract→embed CHAIN (enqueue an embed job once a file has text) is gated
//     on PC_ENABLE_SEMANTIC or the presence of a sidecar, so the co-located
//     extractor can feed a remote GPU embedder without embedding anything itself.
func registerHandlers(ctx context.Context, runner *jobs.Runner, store *jobs.Store, database *db.DB, embedCfg config.EmbedConfig, log *slog.Logger) error {
	fileStore := files.NewStore(database.Pool)

	// Embedding: DB + sidecar only, so it runs anywhere the database is reachable.
	if embedCfg.Enabled() {
		client := embed.NewClient(embedCfg.URL, embedCfg.Model, embedCfg.Dim)
		if verifyEmbedSidecar(ctx, client, embedCfg, log) {
			embedHandler := embed.NewHandler(fileStore, fileStore, client, log)
			runner.Register(embed.Kind, func(ctx context.Context, j jobs.Job) error {
				return embedHandler.Handle(ctx, j.NodeID, j.OwnerID)
			})
			log.Info("embedding handler registered",
				"sidecar", embedCfg.URL, "model", embedCfg.Model, "dim", embedCfg.Dim)
		}
	}

	// Extraction: needs blob content, so only where the blob store is reachable.
	blobPath := os.Getenv("PC_BLOB_PATH")
	if blobPath == "" {
		log.Warn("PC_BLOB_PATH not set; extraction/OCR handler not registered (embedding may still run)")
		return nil
	}

	blobs, err := blob.NewFSStore(blobPath)
	if err != nil {
		return fmt.Errorf("blob store: %w", err)
	}
	filesSvc := files.NewService(fileStore, blobs, log)
	casStore, err := cas.NewStore(database.Pool, blobs)
	if err != nil {
		return fmt.Errorf("content-addressed store: %w", err)
	}
	filesSvc.SetCAS(casStore)

	ex := extract.New()
	extractHandler := extract.NewHandler(files.NewExtractOpener(filesSvc), fileStore, ex, log)
	// Auto-tagging rides along with extraction: every extracted file gets its
	// MIME-category and keyword tags in the same pass.
	extractHandler.Tagging(fileStore)

	// Chain extraction to enqueue an embed job once a file has text — enabled by a
	// local sidecar (this worker also embeds) or by PC_ENABLE_SEMANTIC (this worker
	// only extracts and feeds a separate GPU embedder). The embed job that results
	// is drained by whichever worker registered the embed handler.
	if embedCfg.Enabled() || embedCfg.EnableSemantic {
		extractHandler.Chain(func(ctx context.Context, nodeID *uuid.UUID, ownerID uuid.UUID) {
			if _, _, err := store.Enqueue(ctx, embed.Kind, nodeID, ownerID,
				jobs.EnqueueOptions{OwnerQueueCap: 50000}); err != nil {
				log.Warn("enqueue embed failed", "error", err)
			}
		})
		log.Info("extraction will enqueue embeddings")
	}

	log.Info("extraction handler registered", "ocr_available", ex.HasOCR())
	runner.Register(extract.Kind, func(ctx context.Context, j jobs.Job) error {
		return extractHandler.Handle(ctx, j.NodeID, j.OwnerID)
	})
	return nil
}

// verifyEmbedSidecar checks the sidecar serves the width and model this worker
// is configured to write, reporting whether the embed handler should register.
//
// A dimension mismatch is fatal to the handler: Client.Embed rejects every
// vector of the wrong width, so each job would fail, retry through its whole
// backoff budget and dead-letter. Not registering means those jobs stay queued
// for a correctly configured worker instead of being burned. A model mismatch is
// a warning, because the identity vectors are stored under is operator-chosen —
// but writing a different model's vectors under an existing identity is what
// silently poisons the space, so it must be said loudly.
func verifyEmbedSidecar(ctx context.Context, c *embed.Client, cfg config.EmbedConfig, log *slog.Logger) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	info, err := c.Verify(probeCtx)
	switch {
	case errors.Is(err, embed.ErrDimMismatch):
		log.Error("embedding handler NOT registered: sidecar dimension mismatch",
			"error", err, "configured_dim", cfg.Dim, "sidecar_dim", info.Dim)
		return false
	case err != nil:
		// The sidecar loads a multi-hundred-megabyte model at startup and this
		// worker may well win the race to be ready. Register anyway; a job that
		// runs before it answers fails once and retries with backoff.
		log.Warn("could not verify embedding sidecar at startup; registering anyway",
			"error", err, "sidecar", cfg.URL)
		return true
	}

	if !c.SameModel(info.Model) {
		log.Warn("embedding sidecar model does not match PC_EMBED_MODEL; vectors from two different models would share one identity and be compared as if they were in the same space — set PC_EMBED_MODEL to a new identity and re-index (cloudctl jobs reindex)",
			"sidecar_model", info.Model, "configured_model", cfg.Model)
	}
	log.Info("embedding sidecar verified",
		"model", info.Model, "dim", info.Dim, "device", info.Device)
	return true
}

// serveMetrics exposes /metrics and a /healthz liveness probe.
//
// /healthz reports that this process is alive and nothing more — deliberately
// not that the database is reachable. A liveness probe that fails when a
// dependency is down makes the orchestrator restart a healthy worker, turning a
// Postgres blip into a crash loop, which is the same reasoning the API's probe
// follows.
func serveMetrics(ctx context.Context, addr string, m *metrics.Metrics, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("worker metrics listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("worker metrics server stopped", "error", err)
	}
}

// prune periodically deletes finished jobs so the table does not grow forever,
// and drops jobs that have sat queued past queuedTTL without ever being claimed.
//
// The second sweep exists because a job kind no deployed worker registers is
// never claimed and so never reaches done or failed — it would otherwise
// accumulate one row per upload indefinitely. Dropping such work is lossy, so it
// is logged at warn: the fix is to deploy the worker that handles that kind, not
// to let the sweep keep swallowing it.
func prune(ctx context.Context, store *jobs.Store, retention, queuedTTL time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := store.DeleteFinished(ctx, retention); err != nil {
				log.Warn("prune finished jobs failed", "error", err)
			} else if n > 0 {
				log.Info("pruned finished jobs", "count", n)
			}

			if queuedTTL <= 0 {
				continue
			}
			if n, err := store.DeleteStaleQueued(ctx, queuedTTL); err != nil {
				log.Warn("prune stale queued jobs failed", "error", err)
			} else if n > 0 {
				log.Warn("dropped queued jobs no worker ever claimed; is a handler for their kind deployed?",
					"count", n, "queued_ttl", queuedTTL.String())
			}
		}
	}
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("PC_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.ToLower(os.Getenv("PC_LOG_FORMAT")) == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
