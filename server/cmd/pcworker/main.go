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
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs/billing"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs/tiering"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/media"
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

	// Metering (Phase 9 slice 5). A periodic task rather than a queue job,
	// because it is per-OWNER and per-PERIOD work with no node to key on — and
	// the queue's unique-pending index, its retry budget and its dead letter are
	// all shaped around a job that belongs to one file.
	//
	// It reads the numbers from files.Store.Usage, which is what quota
	// enforcement and GET /usage already answer from, and writes them into a
	// metering record. There is no second accounting here, deliberately: two
	// notions of a number disagree eventually, and a disagreement about how much
	// somebody stored is one that surfaces on a bill.
	if err := startMetering(ctx, database, log); err != nil {
		return fmt.Errorf("billing: %w", err)
	}

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

	// The cold tier, if one is configured. Built here rather than only in the API
	// because this process is where demotion runs, and because every handler
	// below reads content: a worker with a plain local store would fail to open
	// exactly the files that had already been moved.
	coldCfg, err := config.LoadCold()
	if err != nil {
		return fmt.Errorf("cold tier: %w", err)
	}
	_, blobs, err := blob.Open(blobPath, workerColdConfig(coldCfg), log)
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

	// Media: dimensions, EXIF and rendered thumbnails. Registered under the same
	// condition as extraction — it needs file bytes — but as its own kind, so a
	// second co-located worker could drain photos while the first is busy OCRing
	// a stack of scans.
	mediaHandler := media.NewHandler(
		files.NewMediaOpener(filesSvc),
		files.NewMediaStore(fileStore),
		files.NewMediaBlobWriter(filesSvc),
		log,
	)

	// Video thumbnails, only where an operator named an ffmpeg binary. Off by
	// default, and off is a supported state: videos still get their duration,
	// dimensions, rotation and capture time from the container header, so they
	// still take their place in the timeline — they just have no tile, which is
	// exactly what happened before this existed.
	mediaCfg, err := config.LoadMedia()
	if err != nil {
		return err
	}
	thumbnailer := media.NewThumbnailer(mediaCfg.FFmpegPath)
	mediaHandler.VideoThumbnails(thumbnailer)
	if mediaCfg.VideoThumbnailsEnabled() && !thumbnailer.Available() {
		// The one combination worth shouting about: somebody asked for this and
		// the binary is not there. Silently behaving like the default would send
		// them looking for the bug in the job queue.
		log.Warn("PC_FFMPEG_PATH is set but no such executable was found; video thumbnails stay off",
			"path", mediaCfg.FFmpegPath)
	}

	runner.Register(media.Kind, func(ctx context.Context, j jobs.Job) error {
		return mediaHandler.Handle(ctx, j.NodeID, j.OwnerID)
	})
	log.Info("media handler registered",
		"thumb_max_edge", media.ThumbMaxEdge, "preview_max_edge", media.PreviewMaxEdge,
		"video_thumbnails", thumbnailer.Available())

	// Faces: its own kind and its own sidecar, so a deployment that wants
	// thumbnails and search need not run a face model. Registered only where BOTH
	// the blob store and a detector are reachable — it needs image bytes and a
	// GPU, which is the narrowest requirement of any handler here.
	if embedCfg.DetectionEnabled() {
		detector := media.NewDetectClient(embedCfg.DetectURL, embedCfg.DetectModel, embedCfg.DetectDim)
		faceHandler := media.NewFaceHandler(
			files.NewMediaOpener(filesSvc), files.NewFaceStore(fileStore), detector, log)
		runner.Register(media.FaceKind, func(ctx context.Context, j jobs.Job) error {
			return faceHandler.Handle(ctx, j.NodeID, j.OwnerID)
		})
		log.Info("face detection handler registered",
			"sidecar", embedCfg.DetectURL, "model", embedCfg.DetectModel, "dim", embedCfg.DetectDim)
	}

	// Image embedding: the second vector space, and the same narrow requirement
	// as faces — image bytes AND a model. Registered under the identical
	// condition and for the identical reason, so a deployment that wants
	// thumbnails and document search runs neither and loses nothing it had.
	if embedCfg.ImageEmbeddingEnabled() {
		imageClient := embed.NewImageClient(embedCfg.ImageURL, embedCfg.ImageModel, embedCfg.ImageDim)
		if verifyImageSidecar(ctx, imageClient, embedCfg, log) {
			imageHandler := embed.NewImageHandler(
				files.NewImageEmbedOpener(filesSvc), files.NewImageVectorStore(fileStore), imageClient, log)
			runner.Register(embed.ImageKind, func(ctx context.Context, j jobs.Job) error {
				return imageHandler.Handle(ctx, j.NodeID, j.OwnerID)
			})
			log.Info("image embedding handler registered",
				"sidecar", embedCfg.ImageURL, "model", embedCfg.ImageModel, "dim", embedCfg.ImageDim)
		}
	}

	// Tiering: its own kind, so a deployment can run demotion on the box that can
	// reach the bucket and nothing else. Registered only when a cold tier is
	// actually configured — a tier job on a server with no bucket would do
	// nothing but dead-letter, which is the same reason faces is conditional.
	if tiered, ok := blobs.(*blob.TieredStore); ok && tiered.Enabled() {
		tierHandler, err := tiering.NewHandler(
			tiering.NewStore(database.Pool), tiered,
			tiering.Policy{
				MinAge:  coldCfg.MinAge,
				MinIdle: coldCfg.MinIdle,
				MinSize: coldCfg.MinSize,
				Batch:   coldCfg.Batch,
			}, log)
		if err != nil {
			return fmt.Errorf("tiering: %w", err)
		}
		runner.Register(tiering.Kind, func(ctx context.Context, _ jobs.Job) error {
			return tierHandler.Handle(ctx)
		})
		p := tierHandler.Policy()
		log.Info("tiering handler registered",
			"endpoint", coldCfg.Endpoint, "bucket", coldCfg.Bucket,
			"min_age", p.MinAge, "min_idle", p.MinIdle, "min_size", p.MinSize, "batch", p.Batch)
	}

	return nil
}

// workerColdConfig is coldConfig's twin for the worker, which loads the
// cold-tier settings on their own rather than the API's whole configuration.
//
// nil when the tier is off, which is what makes blob.Open return the local
// store unwrapped — the state every deployment without a cold tier is in.
func workerColdConfig(c config.ColdSettings) *blob.S3Config {
	if !c.ColdTierEnabled() {
		return nil
	}
	return &blob.S3Config{
		Endpoint:  c.Endpoint,
		Bucket:    c.Bucket,
		Region:    c.Region,
		AccessKey: c.AccessKey,
		SecretKey: c.SecretKey,
		Prefix:    c.Prefix,
	}
}

// verifyImageSidecar is verifyEmbedSidecar's judgement applied to the image
// space: a DIMENSION mismatch means every vector this worker wrote would be
// invisible to the ranking filter forever, so the handler is not registered and
// the jobs stay queued for a correctly configured worker rather than burning
// their retry budget. Anything else — a sidecar still loading a multi-hundred-
// megabyte vision model — is transient, so it registers anyway and the first job
// retries with backoff. A MODEL mismatch is a warning for the reason the text
// path documents: it is the one failure nothing downstream can detect.
func verifyImageSidecar(ctx context.Context, c *embed.ImageClient, cfg config.EmbedConfig, log *slog.Logger) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	info, err := c.Verify(probeCtx)
	switch {
	case errors.Is(err, embed.ErrDimMismatch):
		log.Error("image embedding handler NOT registered: sidecar dimension mismatch",
			"error", err, "configured_dim", cfg.ImageDim, "sidecar_dim", info.Dim)
		return false
	case err != nil:
		log.Warn("could not verify image embedding sidecar at startup; registering anyway",
			"error", err, "sidecar", cfg.ImageURL)
		return true
	}
	if !c.SameImageModel(info.Model) {
		log.Warn("image sidecar serves a DIFFERENT model than PC_IMAGE_EMBED_MODEL names; "+
			"new vectors will land in the same logical space as another model's",
			"sidecar_model", info.Model, "configured_model", cfg.ImageModel)
	}
	return true
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

// startMetering wires the billing metering sweep and, if configured, the
// outbound billing webhook.
//
// It runs here rather than in the API for the same reason every other periodic
// and memory-hungry thing does: the always-on box's API process keeps its
// request path clear of optional outbound work, and stopping this process
// degrades the system to exactly the one that existed before billing hooks —
// quotas still enforced, /usage still correct, simply no history being recorded.
//
// Zero PC_BILLING_METER_INTERVAL disables it entirely and is a supported state.
// A deployment that will never bill for anything may reasonably decline to pay a
// usage query per account per tick, and the plan endpoints keep working; they
// just have nothing historical to report.
func startMetering(ctx context.Context, database *db.DB, log *slog.Logger) error {
	cfg, err := config.LoadBilling()
	if err != nil {
		return err
	}
	kind, err := billing.ParsePeriodKind(cfg.Period)
	if err != nil {
		return err
	}
	if !cfg.MeteringEnabled() {
		log.Info("metering disabled (PC_BILLING_METER_INTERVAL=0); no usage snapshots will be recorded")
		return nil
	}

	hook := billing.NewWebhook(billing.Config{
		URL:      cfg.WebhookURL,
		Secret:   cfg.WebhookSecret,
		Timeout:  cfg.WebhookTimeout,
		Attempts: cfg.WebhookAttempts,
	}, log)
	if hook != nil {
		log.Info("billing webhook enabled", "url", cfg.WebhookURL, "attempts", cfg.WebhookAttempts)
	}

	meter := billing.NewMeter(
		billing.NewStore(database.Pool),
		files.NewStore(database.Pool),
		hook, kind, log)
	go func() {
		meter.Run(ctx, cfg.MeterInterval)
		// Deliveries already in flight when the context is cancelled are waited
		// for rather than abandoned. Best effort does not mean careless.
		hook.Wait()
	}()
	log.Info("metering enabled", "interval", cfg.MeterInterval.String(), "period", string(kind))
	return nil
}
