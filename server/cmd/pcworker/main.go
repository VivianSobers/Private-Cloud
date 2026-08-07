// Command pcworker drains the background job queue: OCR, embeddings, tagging.
//
// It is a SEPARATE process from the API on purpose. The always-on box is 7 GiB
// and one spinner, and the intelligence features load models the API must never
// hold resident. Running them here means the file and sync API keeps its memory
// budget, and stopping this process degrades the system to exactly the Phase 3
// system — every file endpoint still works, search still works on filenames.
//
// Slice 1 ships the runtime: it connects, reaps jobs abandoned by a crashed
// worker, prunes finished ones, and dispatches to whatever handlers are
// registered. The handlers themselves — OCR, embeddings — arrive in the next
// slices; a worker with none registered idles harmlessly.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs"
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

	// The worker reads only what it needs — the database URL — rather than the
	// API's full config, so it stays independent of WebAuthn and blob settings it
	// has no use for. (The handlers that need blob access will read PC_BLOB_PATH
	// when they are added.)
	dsn := os.Getenv("PC_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("PC_DATABASE_URL is required")
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
		Idle:  envDuration("PC_WORKER_IDLE", 2*time.Second),
		Lease: envDuration("PC_WORKER_LEASE", 10*time.Minute),
	})

	// The handlers need to read file content. Co-located here (same box as the blob
	// store), they read blobs directly; PC_BLOB_PATH is required for that. A GPU
	// worker on another box would instead pull content over the download API —
	// same handlers, different Opener — but that adapter is not wired yet.
	if err := registerHandlers(runner, store, database, log); err != nil {
		return err
	}

	go prune(ctx, store, envDuration("PC_JOB_RETENTION", 7*24*time.Hour), log)

	log.Info("pcworker draining queue")
	if err := runner.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("pcworker stopped")
	return nil
}

// registerHandlers wires the extraction handler, and — when an embedding sidecar
// is configured — the embed handler and the extract→embed chain. Without
// PC_BLOB_PATH the worker still runs (reaping, pruning) but registers no handler,
// so a deployment can defer intelligence by simply not pointing it at the blobs.
func registerHandlers(runner *jobs.Runner, store *jobs.Store, database *db.DB, log *slog.Logger) error {
	blobPath := os.Getenv("PC_BLOB_PATH")
	if blobPath == "" {
		log.Warn("PC_BLOB_PATH not set; no content handlers registered")
		return nil
	}

	blobs, err := blob.NewFSStore(blobPath)
	if err != nil {
		return fmt.Errorf("blob store: %w", err)
	}
	filesSvc := files.NewService(files.NewStore(database.Pool), blobs, log)
	casStore, err := cas.NewStore(database.Pool, blobs)
	if err != nil {
		return fmt.Errorf("content-addressed store: %w", err)
	}
	filesSvc.SetCAS(casStore)

	ex := extract.New()
	extractHandler := extract.NewHandler(files.NewExtractOpener(filesSvc), filesSvc.Store(), ex, log)

	// Semantic search: an inference sidecar embeds document text. When one is
	// configured, register the embed handler and chain extraction to enqueue an
	// embed job once a file has text. Without it, extraction stops at searchable
	// text and no embed jobs are created.
	if embedURL := os.Getenv("PC_EMBED_URL"); embedURL != "" {
		model := envOr("PC_EMBED_MODEL", "bge-small-en-v1.5")
		dim := envInt("PC_EMBED_DIM", 384)
		client := embed.NewClient(embedURL, model, dim)
		embedHandler := embed.NewHandler(filesSvc.Store(), filesSvc.Store(), client, log)
		runner.Register(embed.Kind, func(ctx context.Context, j jobs.Job) error {
			return embedHandler.Handle(ctx, j.NodeID, j.OwnerID)
		})
		extractHandler.Chain(func(ctx context.Context, nodeID *uuid.UUID, ownerID uuid.UUID) {
			if _, _, err := store.Enqueue(ctx, embed.Kind, nodeID, ownerID,
				jobs.EnqueueOptions{OwnerQueueCap: 50000}); err != nil {
				log.Warn("enqueue embed failed", "error", err)
			}
		})
		log.Info("semantic embedding enabled", "sidecar", embedURL, "model", model, "dim", dim)
	}

	log.Info("extraction handler registered", "ocr_available", ex.HasOCR())
	runner.Register(extract.Kind, func(ctx context.Context, j jobs.Job) error {
		return extractHandler.Handle(ctx, j.NodeID, j.OwnerID)
	})
	return nil
}

// prune periodically deletes finished jobs so the table does not grow forever.
func prune(ctx context.Context, store *jobs.Store, retention time.Duration, log *slog.Logger) {
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
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		slog.Warn("ignoring unparseable integer", "key", key, "value", v)
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		slog.Warn("ignoring unparseable duration", "key", key, "value", v)
	}
	return def
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
