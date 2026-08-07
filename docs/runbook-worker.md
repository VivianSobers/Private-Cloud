# Runbook — Background intelligence (worker, OCR, search, embeddings)

**Read this to turn on, monitor, or fix the Phase 4 intelligence layer.** All of
it is optional and additive: with none of it running, every file endpoint works
and search still matches filenames. Turning it off never breaks the file system —
it just stops OCR, content search, semantic search and tagging from updating.

> **Rule zero:** the always-on box is 7 GiB and one spinner. Intelligence runs in
> a *separate* `pcworker` process on purpose; if the box is struggling, stop the
> worker (`docker compose stop pcworker`) and the file API is immediately whole.

---

## Decision table

| What you want | Go to |
|---|---|
| Turn on OCR + content search | [§1 Enable the worker](#1-enable-the-worker) |
| Turn on semantic search | [§2 Enable semantic search](#2-enable-semantic-search) |
| Put embeddings on a GPU box | [§3 GPU split-tier](#3-gpu-split-tier) |
| Check the queue is healthy | [§4 Monitor](#4-monitor) |
| Jobs are stuck / failing | [§5 Troubleshooting](#5-troubleshooting) |
| Re-index everything | [§6 Re-index](#6-re-index) |

---

## 1. Enable the worker

The worker (`pcworker`) drains the job queue: OCR, text extraction, and tagging.
It is part of the compose stack.

```bash
cd deploy/compose
docker compose up -d pcworker
docker compose logs pcworker --tail 50        # look for "extraction handler registered"
```

Uploading a file now enqueues an extraction job; within seconds a scanned receipt
is findable by a word printed on it, through the ordinary search box. The worker's
image carries `tesseract`; if the log says `ocr_available=false`, OCR is off but
text files and PDFs still extract.

## 2. Enable semantic search

Semantic search ("find documents *about* X") needs the embedding sidecar. On a
CPU-only box it works but indexes slowly; a GPU is far better (see §3).

```bash
cd deploy/compose
docker compose --profile semantic up -d embed-sidecar
docker compose exec embed-sidecar curl -s localhost:8000/healthz   # {"device":"cpu"|"cuda", ...}
```

Then in `.env` point the API and worker at it and re-`up`:

```
PC_EMBED_URL=http://embed-sidecar:8000
PC_EMBED_MODEL=bge-small-en-v1.5
PC_EMBED_DIM=384          # MUST equal the model's dimension
```

```bash
docker compose up -d api pcworker
```

The web search box gains a **by meaning** toggle. With no sidecar it reports
"unavailable" and falls back to lexical search — never an error.

## 3. GPU split-tier

Embedding needs only the database and the sidecar — never blob content — so run it
on a 4090 box and keep OCR co-located. Full steps in
[deploy/embed-sidecar/README.md](../deploy/embed-sidecar/README.md). In short:

- Always-on box: `pcworker` keeps `PC_BLOB_PATH`, add `PC_ENABLE_SEMANTIC=true`,
  leave `PC_EMBED_URL` empty. It extracts and enqueues embed jobs.
- GPU box: run the sidecar `--gpus all` and a second `pcworker` with
  `PC_DATABASE_URL` (over the tailnet) and `PC_EMBED_URL` set, **no**
  `PC_BLOB_PATH`. It drains only embed jobs, on the GPU.

The queue's `SKIP LOCKED` claim keeps the two workers from colliding.

## 4. Monitor

- **Metrics:** `privatecloud_jobs{state=...}` (queued/running/done/failed) on the
  API's `/metrics`, polled from the DB every 30 s.
- **Alerts:** `JobQueueBacklog` (queued > 500 for 30 m — worker behind or down),
  `JobsDeadLettering` (any failed for 1 h — something reliably failing).
- **CLI:**

  ```bash
  docker compose exec api /api ...        # the API image; or run cloudctl locally:
  PC_DATABASE_URL=... cloudctl jobs stats     # counts by state
  PC_DATABASE_URL=... cloudctl jobs failed     # dead-lettered jobs and their errors
  ```

## 5. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `queued` climbs, never drains | worker stopped | `docker compose up -d pcworker`; check its logs |
| `failed` count nonzero | a job reliably errors | `cloudctl jobs failed` to see why; fix, then `cloudctl jobs retry` |
| Images not searchable | tesseract missing | worker log `ocr_available=false` — use the provided worker image, not distroless |
| Semantic search 503 | no/unreachable sidecar | `docker compose --profile semantic ps`; check `PC_EMBED_URL` reaches it |
| Semantic returns nothing | dimension mismatch | `PC_EMBED_DIM` must equal the model; a changed model needs a re-index (§6) |
| Worker OOM on the small box | too much concurrent work | it is concurrency 1 by default; if still tight, stop it and run on a bigger box |

Dead-lettered jobs never retry on their own. After fixing the root cause:

```bash
PC_DATABASE_URL=... cloudctl jobs retry      # requeue all failed jobs
```

## 6. Re-index

Extraction and embeddings are content-addressed and idempotent, so re-uploading a
file re-indexes it. To force a clean re-index after changing the embedding model
(a new dimension), drop the affected rows and let the worker rebuild them:

```sql
-- Text (OCR) — rebuilt as files are re-extracted:
DELETE FROM doc_text;
-- Embeddings for an old model:
DELETE FROM doc_embedding WHERE model = '<old-model>';
```

Then re-enqueue extraction for existing files (a small script that inserts an
`extract` job per live file), or simply let new uploads and edits refill it over
time. Search degrades gracefully to filenames while it catches up.
