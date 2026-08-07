# Phase 4 — Intelligence, Identity, Hardening Design

**Status: design.** Written before any code, the discipline that carried Phases 1–3:
make the expensive decisions deliberately, and keep the document as the record of
*why*. Where the code later diverges, the section says so inline.

**Exit criterion:** you can find a scanned receipt by a word printed on it, and a
document by what it is *about* rather than what it is *named* — and none of that
made the box slower to upload a file, hotter, or unable to serve the plain file
API when the clever parts are turned off. A second person can be given an account
through your company's existing single sign-on without minting a passkey by hand.
And the whole surface has been walked once more, deliberately, for the things that
only matter the day someone hostile is looking.

---

## 0. Two tiers, one queue

Two facts about the hardware shape this phase, and they pull in opposite
directions:

- The **always-on box** is **7.2 GiB of RAM, 4 cores, one 500 GB spinner**, ~5 GiB
  already spoken for by the desktop it also is (see the `server-hardware-reality`
  note). ZFS ARC and Postgres are tuned *down* to fit. It has no room for a
  resident model and no GPU.
- There are also **two RTX 4090 boxes** available for compute — separate machines,
  assumed **intermittent** (workstations, not always-on), each with 24 GB of VRAM.

The resolution is a **two-tier split with the job queue as the seam**, and it is
the architecture slice 1 already built rather than a new one:

- **The always-on box owns state**: the API, Postgres (and the job queue in it),
  and the blob store. This tier stays lean — it never loads a model, and turning
  the worker off leaves exactly the Phase 3 system, every file endpoint and
  filename search still working.
- **The 4090 boxes are an accelerator tier**: one or more `pcworker` processes run
  there, reach Postgres over the tailnet, and drain the same queue via
  `FOR UPDATE SKIP LOCKED` — no schema change, because the queue was built for
  exactly this. GPU workers run strong models and batch; a CPU worker on the
  always-on box is the fallback that drains the backlog slowly when the GPUs are
  offline. The queue tolerates their coming and going by construction: jobs wait.

Four rules gate every slice below:

1. **Intelligence is opt-in and out-of-band.** It runs in the worker tier off the
   durable queue, never inline with an upload and never inside the API process. A
   feature that cannot be switched off has no place on the always-on box.
2. **Model choice follows the worker, not the API.** On a GPU worker: a strong
   embedding model (e.g. `bge`/`e5`-class, 384–1024 dim) and GPU-accelerated OCR,
   batched. On the CPU fallback: the small quantized profile (all-MiniLM-L6-v2
   class via ONNX, tesseract). The stored vector's dimension is a config of the
   chosen model, fixed per deployment; the query layer does not care which tier
   produced it.
3. **A remote worker pulls content over the authenticated API, never a mounted
   blob FS.** The GPU box needs a file's bytes to OCR or embed it; it fetches them
   over the tailnet through the same authenticated download path the sync client
   uses (a service credential), so the always-on box stays the only thing that
   touches the blob store directly. State has one owner.
4. **Content never leaves your infrastructure.** A *private* cloud shipping file
   text to a hosted inference API trades away the whole reason it exists. The
   4090 boxes are yours, on your tailnet — local inference or the feature does not
   ship. No hosted API, ever.

**Training is out of scope and not required.** OCR, embeddings and tagging are all
inference over off-the-shelf models; the GPUs make that inference good and fast,
but there is nothing to train for these features. If a later feature genuinely
benefits from fine-tuning (a personalized tagger, say), the GPU tier makes it
possible — but it stays out of Phase 4.

---

## 1. Scope

### In

| | |
|---|---|
| **Job queue + worker** | A durable, DB-backed work queue and a separate `pcworker` process that drains it — the substrate every ML feature rides on |
| **OCR / text extraction** | Pull text out of images and PDFs, feed it into the *existing* trigram search so scanned documents become findable by their words |
| **Semantic search** | Embed document text, store the vectors, and answer "find documents about X" by nearest-neighbour rather than by literal substring |
| **Auto-tagging** | Cheap, explainable tags derived from extracted text and file type — not a black-box image classifier |
| **OIDC login** | An external identity provider as an *additional* way in, alongside passkeys, for onboarding people who already have a company SSO |
| **Hardening pass** | A deliberate second walk of the whole surface: headers, limits, secrets, dependency audit, an abuse review of every endpoint added since Phase 1 |

### Explicitly out

Face recognition and person clustering (a privacy minefield that needs consent
design, not just code); large-language-model summarization or chat over your
files (no resident-model budget on this box, and a different product); training or
fine-tuning anything; real-time OCR as you scroll. Auto-tagging is deliberately
the *cheap* kind — keywords from text, MIME-derived categories — not a vision
model guessing at photo contents.

---

## 2. The shape

```
upload ──► file stored ──► enqueue(extract) ─┐
                                             │   (DB job queue: SKIP LOCKED)
                          pcworker  ◄─────────┘
                             │
              ┌──────────────┼───────────────┐
          tesseract      MiniLM/ONNX      derive tags
              │              │                │
          doc_text      doc_embedding      node_tags
              │              │                │
        trigram search   vector KNN       tag filter
```

The API process enqueues a job when a file lands and otherwise knows nothing about
ML. `pcworker` is the only thing that loads a model, and it is a separate binary
in the same module — the modular-monolith rule from Phase 0 (extractable, not
distributed-by-default) applied to compute instead of routing. It runs where the
compute is: on a GPU box over the tailnet, or on the always-on box as a CPU
fallback. `FOR UPDATE SKIP LOCKED` means one worker or several drain the same
queue with no schema change, and a worker on another machine reaches file bytes
through the authenticated download API, never a mounted blob store.

---

## 3. The job queue and worker (slice 1)

Everything else needs a way to say "do this later, durably, without blocking the
request." Postgres is already here and already the source of truth, so the queue
is a table, not a broker.

- A `jobs` table: `(id, kind, node_id, owner_id, state, attempts, run_after,
  last_error, created_at)`. `state` moves `queued → running → done | failed`.
  Claiming a job is `UPDATE ... WHERE id = (SELECT id ... WHERE state='queued' AND
  run_after <= now() ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
  RETURNING` — the same in-transaction claim that makes the change journal's
  counter gap-free, reused so two workers never take the same job.
- **Idempotent by content, not by job.** A job carries the node id; the worker
  re-reads the node's current version before acting and writes results keyed by
  *content hash*. A file that changed between enqueue and execution is handled at
  its current state, and a job that runs twice writes the same row twice
  harmlessly — the same self-healing property the journal has.
- **Retry with backoff and a dead-letter state.** `attempts` bounds retries;
  `run_after` pushes a failed job out exponentially; a job that exhausts retries
  lands in `failed` with `last_error` rather than looping forever and pinning a
  core the box cannot spare.
- **Throttled and killable.** `pcworker` processes one job at a time by default,
  with a configurable concurrency of 1 on this hardware, and honours SIGTERM
  mid-job by finishing the current step and exiting. Its memory ceiling is the
  operator's lever: stop the worker and the queue simply grows until it runs
  again.

---

## 4. OCR and text extraction (slice 2)

The cheapest intelligence with the highest payoff: a scanned receipt or a
photographed page is invisible to filename search, and OCR makes it findable
without any embedding model at all.

- The worker extracts text per type: **tesseract** for images, a text-layer pull
  (with tesseract fallback for scanned pages) for PDFs, and a plain decode for
  text files. Extraction shells out to tesseract as a subprocess — no cgo, no
  resident OCR engine, and a crash in the C library takes the subprocess, not
  `pcworker`.
- Results land in `doc_text (node_id, content_hash, text, lang, extracted_at)`,
  keyed by content hash so a re-uploaded identical file is not re-OCR'd, and a new
  version supersedes cleanly.
- **This feeds the search that already exists.** Phase 1 built trigram search over
  names and paths; slice 2 extends the same index to `doc_text`, so "find the word
  on the receipt" is the *same* query path, not a new one. Semantic search
  (slice 3) is the addition; keyword search over OCR text is the quick win and
  needs no model.
- Extraction is bounded: a page/size cap and a per-job timeout, because a
  maliciously enormous PDF must not let one upload occupy the single worker for an
  hour.

---

## 5. Semantic search (slice 3)

"Find documents *about* invoices" where the word "invoice" appears nowhere — the
part that genuinely needs a model, and the part the hardware most constrains.

- The worker embeds `doc_text`, chunking long documents and storing one vector per
  chunk in `doc_embedding (node_id, content_hash, chunk_seq, vector, ...)`. The
  model follows the tier (§0): a strong `bge`/`e5`-class model on a GPU worker, the
  small quantized all-MiniLM-L6-v2 class via ONNX on the CPU fallback. The vector
  dimension is fixed per deployment by the chosen model and recorded alongside the
  vector, so a later model change is a re-index, not silent corruption of a mixed
  index.
- **Storage and search: pgvector if present, brute-force if not.** At personal
  scale — thousands of documents, not millions — an exact cosine scan over stored
  vectors is fast enough and needs no extension. pgvector with an HNSW index is
  the clean upgrade and the design targets it, but the query layer is written so
  that its absence degrades to an exact scan rather than a missing feature. The
  base Postgres image gains the extension; the fallback keeps a dev box that lacks
  it working.
- **Ranked, hybrid, honest.** A search blends trigram (lexical) and vector
  (semantic) results, and every result still says *why* it matched — the Phase 1
  principle that "the query is not in this filename" must have a visible answer,
  extended to "matched semantically" so a result that shares no words is not
  mistaken for a bug.
- Embedding is the most expensive step and the most deferrable: it runs at the
  back of the queue behind OCR, and turning the worker off leaves keyword and OCR
  search fully working. On this box that is the expected steady state much of the
  time.

---

**Slice 3 semantic-search notes, recorded where the next reader will look:**

- The model runs in a **Python inference sidecar** (`deploy/embed-sidecar`,
  sentence-transformers), not in Go. With the 4090s available this is the
  realistic GPU path — GPU ONNX-in-Go is awkward — and it keeps the model out of
  every Go process. The Go worker calls it to embed documents; the Go API calls it
  to embed a query. An RPC to a sidecar is not a resident model, so rule §0.1
  holds. Both processes are wired with `PC_EMBED_URL/MODEL/DIM`.
- Vectors are stored as **packed little-endian float32** and searched by an exact
  cosine scan in Go — no pgvector, so it runs on stock Postgres and is exactly
  correct at personal scale. `SemanticSearch` filters by **both model and
  dimension**, so a model re-trained to a new width while keeping its name leaves
  old vectors that are simply not returned, never mismatched into a zero score —
  a mixed store degrades to fewer results, never wrong ones.
- Extraction **chains** into embedding: once a file has text, the extract handler
  enqueues an `embed` job (when the chain is wired). The two are separate job
  kinds so they can run on separate workers — extraction on the always-on box,
  embedding on a GPU box — each claiming only its own kind. Embedding is
  content-addressed and idempotent, like extraction.
- The semantic endpoint is **off cleanly** without a sidecar: `503
  semantic_unavailable`, while lexical and OCR search are untouched. The feature
  is strictly additive, top to bottom.

## 6. Auto-tagging (slice 3b)

Deliberately the cheap, explainable kind.

- Tags come from **extracted text and file type**, not a vision model: keyword and
  entity extraction over `doc_text` (dates, amounts, a small keyword vocabulary),
  plus MIME-derived categories (image / document / spreadsheet / code). Stored in
  `node_tags (node_id, tag, source, confidence)` with `source` recording whether a
  human or the worker applied it.
- **Explainable and reversible.** Every automatic tag names its source, a user can
  remove one, and a removed tag is not re-applied — an auto-tagger that fights the
  user is worse than none. No black-box guess about photo contents, which on this
  hardware would also be the priciest thing to run.

---

## 7. OIDC login (slice 4)

Passkeys stay the primary, phishing-resistant credential. OIDC is an *additional*
door for people who already have a company identity provider, so onboarding a
second user does not mean hand-minting a passkey.

- Standard authorization-code flow with PKCE against a configured provider; on
  first successful login a user is provisioned (or linked to an existing account by
  verified email), and issued the same session the passkey path issues.
- **It does not weaken what exists.** OIDC is opt-in per deployment and off by
  default; the recovery-code and passkey paths are unchanged; an OIDC session is an
  ordinary web session, distinct from the confined device session the sync client
  uses. Admin bootstrap remains passkey-first, so a misconfigured provider cannot
  lock the owner out of their own server.
- Provider config (issuer, client id/secret, allowed domains) is env/secret
  material like everything else, never committed.

**Slice 4 OIDC notes, recorded where the next reader will look:**

- Token verification is delegated to **go-oidc** (JWKS fetch, signature, iss/aud/
  exp) rather than hand-rolled — a subtly wrong JWT check is exactly how an SSO
  integration becomes an auth bypass. `oauth2` provides the code exchange and PKCE
  helpers. Both are configured behind `PC_OIDC_*` env vars, read in `cmd/api`.
- The three transient secrets — **state** (CSRF, compared constant-time against
  the query param), **nonce** (bound into the ID token and checked after verify),
  and the **PKCE verifier** — ride in one short-lived, HttpOnly, SameSite=Lax flow
  cookie scoped to `/api/v1/auth/oidc`, single-use (cleared on callback).
- OIDC **provisions its own users**, keyed by `(issuer, subject)` — the only
  identifier a provider promises stable. It does NOT auto-link to a passkey
  account by email, which removes email-reassignment account takeover as a risk.
  Provisioned users are non-admin; the admin is still a passkey.
- The endpoints are **absent in effect without config**: `404 oidc_disabled`, and
  `/auth/status` advertises `oidc_enabled` so the login page shows an SSO button
  only when it will work. Passkey, recovery and device-token paths are untouched.

---

## 8. Hardening pass (slice 5)

Not a feature — a deliberate second walk of a surface that has tripled since
Phase 1, done once, on purpose, while the whole system is fresh in mind.

- **Every endpoint added since Phase 1, re-reviewed for abuse:** the share plane,
  the delta protocol's chunk endpoints, the device-token exchange, and now the
  job/OCR surface. Rate limits, resource caps, and authorization scoping checked
  against a written list, not from memory.
- **Dependency and secret audit:** `govulncheck` in the loop, the new ML
  dependencies (onnxruntime, tesseract) pinned and their footprint justified,
  every secret confirmed to load from env/secret files and never a default.
- **Resource-exhaustion review, weighted for this box:** the worker's memory
  ceiling, the OCR/PDF size and time caps, the vector-scan cost at the largest
  plausible corpus — the failure modes that a 7 GiB machine hits first.
- **The abuse cases the ML surface newly introduces:** a crafted file that makes
  OCR or embedding pathologically slow (a zip-bomb-shaped PDF), a job queue an
  authenticated user could flood, extracted text as a stored-XSS or log-injection
  vector. Each gets a bound or an escape, written down here.

---

## 9. Slices

Same discipline as before: each slice ends green, committed, and useful on its
own. Intelligence is additive — every slice leaves the plain file and sync system
fully working with the worker turned off.

| Slice | Contents | Status |
|---|---|---|
| **1** | Job queue (`jobs` table, `SKIP LOCKED` claim, retry/backoff) + `pcworker` process | ✅ transactional claim so two workers never share a job; unique-pending index dedups per (kind, node); exponential backoff to a dead-letter state; a reaper returns a crashed worker's job to the queue; `pcworker` is a separate process that idles harmlessly with no handlers registered |
| **2** | OCR / text extraction into `doc_text`, folded into the existing trigram search | ✅ content-addressed `doc_text` (extract once, shared by identical files); a pure `extract` package — text decode, tesseract-subprocess OCR with graceful skip when absent, best-effort PDF text layer; the `extract` job handler and its co-located content Opener; extraction enqueued on upload and folded into the same trigram search with a `matched_content` flag |
| **3** | Semantic search: embeddings via an inference sidecar, brute-force cosine KNN | ✅ content-addressed `doc_embedding` (packed float32, no pgvector needed on stock Postgres); a pure `embed` package (vector math, chunking, sidecar HTTP client); the embed job chained after extraction; a Python sidecar (`deploy/embed-sidecar`) that runs the model on a GPU box; `GET /search?semantic=true` embeds the query and ranks by cosine, off cleanly when no sidecar is wired |
| **3b** | Auto-tagging: MIME-category and keyword tags over extracted text, explainable and reversible | ✅ deterministic `Tags(mime, text)` — MIME category plus a small curated keyword vocabulary, no classifier; per-node `node_tags` (auto + user, `source`-tracked) where re-tagging replaces only auto tags and never a user's; applied in the extraction pass so every file is tagged; per-node tag CRUD, a tag list with counts, and filter-by-tag, with tags riding along in the node GET |
| **4** | OIDC login alongside passkeys, opt-in and non-weakening | ✅ authorization-code + PKCE via vetted `go-oidc`/`oauth2`; state (CSRF), nonce (replay) and PKCE all bound through a short-lived flow cookie; provisions its OWN users keyed by (issuer, subject), never auto-linking a passkey account by email; gates on verified email and an optional domain allowlist; OIDC users are non-admin (admin stays passkey-bootstrapped); a discovery failure disables SSO with a log line rather than aborting startup |
| **5** | Hardening pass: abuse review of every post–Phase-1 endpoint, dep/secret audit, resource caps | ⬜ |

---

## 10. Risks

| Risk | Mitigation |
|---|---|
| ML makes the box unusable for its actual job (serving files) | Everything ML is a separate, throttled, killable worker off a durable queue, and it runs on the GPU tier off-box entirely; the API never loads a model; turning the worker off restores the Phase 3 system exactly |
| A model exhausts the always-on box's 7 GiB | The heavy model runs on a GPU worker, not the always-on box; the CPU fallback uses the small quantized profile at concurrency 1, and the worker's memory ceiling is an operator lever |
| A remote GPU worker needs the blob filesystem | It does not — it pulls file bytes over the authenticated download API on the tailnet, so the always-on box remains the sole owner of the blob store |
| Content leaks to a hosted inference API | Local inference only, on your own tailnet (the always-on box or the 4090 boxes); sending file text off your infrastructure is disqualifying for a private cloud, so the option does not exist in the design |
| A crafted file makes OCR/embedding pathologically slow | Per-job size/page caps and timeouts; a job that exhausts retries dead-letters rather than pinning the one spare core |
| A user floods the job queue | Enqueue is bounded per owner; the queue is drained at a fixed rate, so a flood delays that user's own indexing, not the system |
| pgvector missing on a dev box breaks search | The query layer degrades to an exact cosine scan; the feature works without the extension, just slower |
| OIDC misconfiguration locks the owner out | OIDC is opt-in and additive; passkey and recovery paths are unchanged; admin bootstrap stays passkey-first |
| Extracted OCR text becomes a stored-XSS or log-injection vector | Treated as untrusted content everywhere it surfaces, escaped on display and sanitized in logs, in the slice-5 review |
