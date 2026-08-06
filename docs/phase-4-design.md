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

## 0. The constraint that shapes everything

This phase is where the temptation to add a resident ML stack is strongest, and
where this particular machine says no. The always-on box is **7.2 GiB of RAM, 4
cores, one 500 GB spinner**, with roughly 5 GiB already spoken for by the desktop
it also is (see the `server-hardware-reality` note). ZFS ARC and Postgres are
already tuned *down* to fit. There is no room for a language model sitting
resident in memory, and no second disk to hide random-read latency behind.

Three rules fall out of that, and they gate every slice below:

1. **Intelligence is opt-in and out-of-band.** OCR and embeddings run in a
   **separate worker process** off a durable job queue, never inline with an
   upload and never inside the API process. Turn the worker off and the system is
   exactly the Phase 3 system — every file API still works, search still works on
   filenames. A feature that cannot be switched off has no place on hardware this
   tight.
2. **Models are small, quantized, on-demand, and CPU-only.** Tesseract for OCR; a
   ~90 MB quantized sentence-embedding model (all-MiniLM-L6-v2 class, ONNX via
   `onnxruntime`) loaded by the worker, used, and — under memory pressure —
   released. No GPU is assumed because there is none.
3. **Content never leaves the box.** This is a *private* cloud; shipping file text
   to a hosted embedding API to save a few hundred megabytes of RAM would trade
   away the entire reason the project exists. Local inference or the feature does
   not ship.

The honest consequence: on *this* box, semantic search is a modest convenience
that indexes slowly in the background, not a headline feature. On the eventual
16 GB mirror-backed target it becomes comfortable. The design must be good on both
and degrade gracefully on the small one — which is exactly why the worker is a
separate, throttled, killable process.

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
distributed-by-default) applied to compute instead of routing. One machine, one
queue, `FOR UPDATE SKIP LOCKED` so the worker can be run as more than one process
later without changing the schema.

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

- The worker embeds `doc_text` with a small quantized model (all-MiniLM-L6-v2
  class, 384-dim, ONNX via `onnxruntime-go`), chunking long documents and storing
  one vector per chunk in `doc_embedding (node_id, content_hash, chunk_seq,
  vector, ...)`.
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

## 6. Auto-tagging (slice 3, alongside embeddings)

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
| **2** | OCR / text extraction into `doc_text`, folded into the existing trigram search | ⬜ |
| **3** | Semantic search (embeddings + pgvector-or-brute-force KNN, hybrid ranking) and cheap auto-tagging | ⬜ |
| **4** | OIDC login alongside passkeys, opt-in and non-weakening | ⬜ |
| **5** | Hardening pass: abuse review of every post–Phase-1 endpoint, dep/secret audit, resource caps | ⬜ |

---

## 10. Risks

| Risk | Mitigation |
|---|---|
| ML makes the box unusable for its actual job (serving files) | Everything ML is a separate, throttled, killable worker off a durable queue; the API never loads a model; turning the worker off restores the Phase 3 system exactly |
| A resident model exhausts 7 GiB of RAM | Small quantized CPU model, loaded on demand and released under pressure, concurrency 1; the worker's memory ceiling is an operator lever |
| Content leaks to a hosted inference API | Local inference only; sending file text off-box is disqualifying for a private cloud, so the option does not exist in the design |
| A crafted file makes OCR/embedding pathologically slow | Per-job size/page caps and timeouts; a job that exhausts retries dead-letters rather than pinning the one spare core |
| A user floods the job queue | Enqueue is bounded per owner; the queue is drained at a fixed rate, so a flood delays that user's own indexing, not the system |
| pgvector missing on a dev box breaks search | The query layer degrades to an exact cosine scan; the feature works without the extension, just slower |
| OIDC misconfiguration locks the owner out | OIDC is opt-in and additive; passkey and recovery paths are unchanged; admin bootstrap stays passkey-first |
| Extracted OCR text becomes a stored-XSS or log-injection vector | Treated as untrusted content everywhere it surfaces, escaped on display and sanitized in logs, in the slice-5 review |
