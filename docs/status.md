# Private Cloud — the one status document

Every phase, every slice, one legend, one page. Tables only: what exists, whether
it is done, and — for anything not done — the one line that says why.

The other documents in `docs/` are the ones this cannot replace: the
[Phase 0 operator checklist](phase-0-checklist.md), the
[API contract](api-contract.md) and [openapi.yaml](openapi.yaml), the three
runbooks, [tailscale-setup.md](tailscale-setup.md) and
[custom-metrics.md](custom-metrics.md).

## Legend

| Mark | Means |
|---|---|
| ✅ | **Done** — built, wired end to end, covered by tests in this repo |
| 🟠 | **Partial** — one half exists, or it works with a stated limit; the row says which half |
| ❌ | **Not built** — no code. Deliberately deferred (reason given) or blocked outside this repo |

A phase is ✅ only when the endpoint **and** the thing that consumes it both exist.
That rule is why the later phases get two columns.

**Where the marks come from:** the server route table (`server/internal/httpapi/*.go`),
`awaitingClient` in [contract_test.go](../server/internal/httpapi/contract_test.go)
(fails both when a route is unconsumed *and* undeclared, and when a declaration goes
stale), the migration and test trees, `web/src`, `client/`, and `deploy/` + `scripts/`.
[CI](../.github/workflows/ci.yml) re-runs every executable claim on each push.

---

## Roll-up

| Phase | Scope | Behind the API | In front of it | Overall |
|---|---|---|---|---|
| 0 | Storage, network, monitoring, backups, runbooks | ✅ all of it is code; CI proves the restore path | — | ✅ |
| 1 | MVP: auth, files, resumable upload, web UI, WebDAV, search | ✅ 7/7 | ✅ | ✅ |
| 2 | CAS engine, versioning, dedup, share links | ✅ 4/4 | ✅ | ✅ |
| 3 | Sync engine: journal, delta protocol, Go client, conflicts | ✅ 4/4 | ✅ `pcsync` | ✅ |
| 4 | OCR, semantic search, tagging, OIDC, hardening | ✅ 6/6 | ✅ | ✅ |
| 5 | Photos & media: EXIF, thumbnails, albums, timeline, map | ✅ 8/8 | ✅ gallery, lightbox, albums, drag-reorder, map | 🟠 video metadata only |
| 6 | Native clients: desktop tray, mobile/PWA | ✅ devices; ❌ push sender | 🟠 PWA + share target ✅; tray shell, signed installers ❌ | 🟠 |
| 7 | Multi-user, sharing, RBAC, admin, quotas | ✅ 4/4 | 🟠 sharing, admin, quotas ✅; **browsing into a granted folder** ❌ | 🟠 |
| 8 | Advanced intelligence: faces, similar files, RAG chat | 🟠 4/5 — image similarity ❌ | 🟠 chat, people, find-similar ✅; **SSE not consumed** | 🟠 |
| 9 | Scale & resilience: cold tier, DR automation, quotas | 🟠 storage health, quota, DR drill ✅; cold tier, billing ❌ | ✅ admin Storage tab | 🟠 |

Phases 1–4 are finished on both sides. Phases 5–9 are finished behind the API
except image similarity, the cold tier and billing. **Nothing left is blocked on a
decision** — the open client items need an environment (a GUI to compile a tray
against, code-signing keys) or are small and unclaimed; the open server items are
deliberate deferrals.

### Landed since this document was last derived

| Item | Commit | Now |
|---|---|---|
| Map view over photo GPS | `4875cb5` | ✅ `web/src/geo.ts` + `Photos.tsx`, no tile provider — nothing phones home |
| Pointer-drag album reordering | `614c6e1` | ✅ still one wholesale `PATCH /albums/{id}/items` |
| PWA share target | `5a557a1` | ✅ manifest + `ShareTarget.tsx` + SW handler |
| Streaming chat answers | `bd59bf6` | ✅ server-side SSE, citations first; `Ask.tsx` does not consume it yet |
| DR rehearsal on a schedule | `0fdce2d` | ✅ `scripts/dr-drill.sh`, timer, alert rules |
| Per-user API rate limiting | `8c7f792` | ✅ the item this document had carried longest as "most overdue" — built, wired in `requireAuth`, tested |
| pgvector / HNSW index | *this change* | ✅ optional: stock Postgres keeps the exact scan, pgvector gets SQL ranking and an HNSW index per vector width |
| Generation + detection sidecars | `fafa1d0` | ✅ both reference images committed — Dockerfile, `app.py`, `requirements.txt` each |

---

## What is not done — the whole open list

| # | Item | Phase | Mark | State, and what it needs |
|---|---|---|---|---|
| 1 | **Browsing into a granted folder** | 7 | ❌ | Sequencing only. The server has supported `?include_shared=true` since Phase 7 slice 2 and `api.ts` can send it; no view opts in, so a grantee reaches shared content through `/shared` and `/chat` but cannot open a shared *folder* inline |
| 2 | **Streaming consumed in the browser** | 8 | 🟠 | `POST /chat` serves `text/event-stream` with citations before prose; [Ask.tsx](../web/src/Ask.tsx) still awaits the whole body |
| 3 | **Platform tray icon + menu adapter** | 6 | ❌ | Blocked on an **environment**, not a decision: needs a CGO system-tray library and a machine with a display. Everything it would render and every action it would fire is built and unit-tested in `client/internal/tray` |
| 4 | **Push delivery (VAPID key + sender)** | 6 | ❌ | Both halves are **behind** the API: a PWA cannot call `PushManager.subscribe` without a public key the server does not publish, and nothing would deliver if it could. A client that registers nothing polls `GET /changes`, so push is latency, never correctness |
| 5 | **Signed installers + auto-update** | 6 | 🟠 | Cross-built binaries + `SHA256SUMS` ✅, unsigned `.deb`/`.rpm` ✅ (unsigned **by design** — a local install needs no repo signature, only a *repository* does). ❌ A signed apt/dnf repo, Homebrew/Scoop, `.msi`/`.pkg` and an in-place updater need code-signing keys and per-OS tooling outside this repo |
| 6 | **Image-embedding similarity for photos** | 8 | ❌ | `/similar` works through the text-embedding space, so two photographs with no text have no neighbours. Needs a fourth model and a second vector space; most of the value is already delivered by face clustering and the timeline |
| 7 | **Object-storage cold tier** | 9 | ❌ | Not started, and honest about it: `GET /admin/storage` reports `tiering.enabled: false` rather than a cold tier holding zero bytes. **By design** until `fsck` can be taught a third location |
| 8 | **DR recovery automation** | 9 | 🟠 | The **drills** are automated (`scripts/dr-drill.sh` + `scripts/restore-drill.sh`, timers, alerts, run by CI against a real pool). The **recovery procedures** stay manual deliberately: automating a restore means automating something whose failure mode is overwriting good data with old data, under time pressure, with no second chance |
| 9 | **Video metadata beyond "this is a video"** | 5 | ❌ | `analyzeVideo` records the kind and nothing else — no duration, dimensions, rotation or thumbnail. Those live in MP4/MKV boxes needing a real demuxer; both honest options (cgo ffmpeg, or shelling out) belong behind the same opt-in switch OCR sits behind |
| 10 | **Feedback controls that feed labels back** | 8 | ❌ | Unbuilt on both sides and needs a decision first: a face correction is already permanent (`faces.dismissed_at`), so "feedback" here means labels that retrain a model, which Phase 4 put out of scope |
| 11 | **Billing hooks** | 9 | ❌ | Not started. There is no second tenant; quota exists and is enforced, and the thing billing would attach to is one person's disk |
| 12 | **Encrypted pool auto-unlock** | 0 | 🟠 | **Decided, deliberately not enabled.** The unit exists and documents what each keyfile location costs; `scripts/zfs-unlock.sh` refuses a key on the root filesystem, because storing the key beside the ciphertext is not a weaker setup, it is no setup. Cost: a remote reboot needs a console |
| 13 | **`restore-test.sh` against *your* pool** | 0 | 🟠 | Operator gate. CI proves the restore path against a loopback pool on every push; only you can prove *your* disks do |
| 14 | **Last-admin guard refusal test** | 7 | 🟠 | The guard exists and its succeeding path is tested; its **refusal** path has no integration test, because the condition is a global property of the `users` table that every fixture's second admin defeats. Recorded, not papered over |

### Served, with no client

| Route / parameter | Waiting on |
|---|---|
| `POST`/`DELETE /devices/{id}/push` | a VAPID public key the server does not publish, and a sender that does not exist — **both behind the API**, so this is not waiting on a UI |
| `?include_shared=true` (children, search, tags) | a caller. Not covered by the contract test, because it is a query parameter rather than a route |

---

## Phase 0 — Foundation ✅

**Exit criterion:** ZFS pool healthy · Tailscale connected · Docker stack up ·
Grafana showing data · ntfy alert received on your phone · restore test executed.
Everything unticked needs `sudo` on the real server — the procedure is
[phase-0-checklist.md](phase-0-checklist.md).

| Item | Status |
|---|---|
| ZFS pool + dataset layout (`scripts/zfs-setup.sh`) | ✅ |
| Snapshot ladder (`scripts/sanoid-setup.sh`, `deploy/sanoid/`) | ✅ |
| Tailscale-only plane, zero forwarded ports (`deploy/caddy/Caddyfile`) | ✅ |
| Docker stack: Postgres, Caddy, Prometheus, Grafana, Alertmanager, ntfy, exporters | ✅ Postgres is a derived image (`Dockerfile.postgres`) carrying pgbackrest and pgvector |
| Nightly encrypted restic backup + freshness metric | ✅ |
| Pool-health textfile collector + systemd timer | ✅ |
| Alert rules with rule tests (`alerts.yml`, `alerts_test.yml`) | ✅ 39 rules, 7 of them guarding the two drills |
| Runbooks: [restore](runbook-restore.md) · [disaster recovery](runbook-disaster-recovery.md) · [worker](runbook-worker.md) | ✅ |
| Restore drill automated (`restore-drill.sh` + monthly timer + 3 alerts) | ✅ CI runs it against a real ZFS pool on loopback vdevs every push |
| DR runbook rehearsal automated (`dr-drill.sh` + timer + alerts) | ✅ |
| `scripts/restore-test.sh` executed against **your** pool | 🟠 open item 13 |
| Grafana dashboards committed and self-provisioning | ✅ five, incl. a Private Cloud overview; CI parses all 360 queries |
| Images pinned to digests (+ Renovate to move them) | ✅ all ten third-party images; CI fails on an unpinned one |
| Real TLS via `tailscale cert` instead of `tls internal` | ✅ script + weekly timer; CI validates the Caddyfile in both states |
| UPS + NUT · unattended security upgrades | ✅ `deploy/host/nut/` · `deploy/host/apt/`, security origins only, ZFS/kernel blacklisted |
| pgBackRest point-in-time recovery | ✅ RPO 24h → one WAL segment; [runbook-restore.md](runbook-restore.md) §4c |
| Encrypted pool auto-unlock | 🟠 open item 12 |
| Backup-freshness metric · pool-health metric | ✅ both |
| Host-level install in one command (`scripts/host-setup.sh --all` / `--check`) | ✅ |
| CI: build, vet, race, govulncheck, contract, dashboards, alert rules, Caddy, compose, shellcheck, drills | ✅ |

| Decision | Why |
|---|---|
| A pinned digest gets Renovate | A pinned digest with nothing to update it is a security problem wearing a reproducibility costume |
| The UPS is not about uptime | ZFS survives one power cut by design; what it survives badly is the second and third during the resilver after the first |

---

## Phase 1 — MVP ✅ 7/7, both sides

**Exit criterion:** you stop using Google Drive for manual file workflows — not "the
API works", but you keep real files here because it is the more convenient option. **Met.**

| # | Slice | Status |
|---|---|---|
| 1 | Skeleton: config, DB, migrations, `/healthz`, `/metrics`, structured logs | ✅ |
| 2 | Auth: passkeys, sessions, recovery codes, admin bootstrap, `cloudctl` | ✅ |
| 3 | Files core: tree CRUD, upload, Range download, trash, `fsck` | ✅ |
| 4 | Resumable upload (tus) | ✅ |
| 5 | Web UI: browse, upload, download, preview, trash | ✅ |
| 6 | WebDAV | ✅ |
| 7 | Search: ranked trigram over name and path | ✅ |
| — | App passwords — unplanned; WebDAV cannot run a WebAuthn ceremony | ✅ |

| Decision still load-bearing | Why |
|---|---|
| Versioned schema + `blob.Store` built a phase early | Phase 2's CAS migration was "change what `file_versions` points at", not a schema rewrite over live data |
| `name_fold` + `UNIQUE (parent_id, name_fold)` from migration 1 | Mac and Windows clients happily create `Photos` beside `photos`; retrofitting means reconciling real collisions |
| `path` cached alongside `parent_id` | The adjacency list is truth; the materialised path makes Phase 7's inherited ACLs a prefix test. Cost: a rename rewrites the subtree |
| Blob first, then the DB row — never the reverse | A crash leaves a GC-able orphan, not a row pointing at bytes that do not exist. `fsck` exists from slice 3 to prove it |
| Recovery codes + `cloudctl user reset-auth` are non-negotiable | Passkey lockout is real; no reset email, no second admin, no service to ask. Root on the box is already total access |
| Auth is slice 2, before files | Retrofitting authorization into handlers that assumed one user is miserable and error-prone |
| Search ranks, it does not just filter | A bare `LIKE` over a trigram index returns table order, useless past a screenful. Shipped: exact → prefix → similarity → recency |
| stdlib `net/http`; no job queue in Phase 1 | One less dependency in the auth path; a queue before there is a job is infrastructure for its own sake — it arrives in Phase 4 with a reason |

---

## Phase 2 — Storage engine ✅ 4/4, both sides

**Exit criterion:** recover a file you overwrote three weeks ago, send someone a link
to a folder without giving them an account, and hold noticeably more than the sum of
the bytes you uploaded. **Met.**

| Slice | Contents | Status |
|---|---|---|
| 1a | Chunk store: FastCDC + BLAKE3 + zstd behind `blob.Store`; both formats coexist | ✅ `internal/cas` |
| 1b | All three write paths (direct, resumable, WebDAV) route through the chunker | ✅ |
| 2 | Chunk GC, refcount recompute, CAS-aware `fsck`, blob migration, dedup stats | ✅ |
| 3 | Version history: list, restore, retention, UI | ✅ |
| 4 | Share links: public plane, tokens, password, expiry, cap, revocation, UI | ✅ |

| Rule | Detail |
|---|---|
| Chunking is protocol, not implementation | FastCDC min 2 KiB / target 16 KiB / max 64 KiB, BLAKE3-256, zstd-3 — Phase 3's client re-declares them. Sub-minimum files stay whole blobs; compressed content skips zstd, detected by MIME + entropy and recorded per chunk so the reader never guesses |
| Refcount errors resolve toward the overcount | An undercount is unrecoverable data loss, an overcount is wasted disk — not symmetric. Trigger-maintained, verified inside the deleting transaction, recomputable by `fsck`, which reports drift without acting on it |
| `fsck` learned chunks *before* anything wrote them | Chunks share the `ab/cd/hash` layout with blobs, so a blob-only checker calls every deduplicated byte an orphan and `--repair` deletes it. Phase 5 hit the identical trap with media variants |
| Migration `00008` taught the trigger **UPDATE** | Otherwise the in-place blob→manifest switch moves the reference off the old blob without decrementing it, and GC leaks every migrated blob forever |
| Blob migration is an in-place UPDATE, never a new version | History untouched; the API cannot tell a migrated file from a native one. A version whose bytes are already gone is **failed, never repointed** |
| GC order is manifests before chunks | One pass reclaims a purged file all the way to its bytes |
| Restore is an **append** | A new version pointing at the target's existing content — so a 4 GB rollback is one row and zero bytes, and is itself undoable. Retention prunes only versions failing **both** tests, never the head, guarded by id rather than rank |
| A share is a **row**, never a signed token | Revocation must be immediate. The URL token is 256 bits, stored SHA-256 hashed, returned exactly once at creation |
| Password unlock keeps no server session | argon2id verification returns an HMAC over a per-share `unlock_key`, path-scoped, so a proof for one share cannot open another and the cookie is never transmitted elsewhere |
| The download cap is enforced in the increment | `UPDATE ... WHERE download_count < max_downloads` — concurrent downloads racing the last slot cannot both win |
| Folder shares are confined twice over | Owner-scoped lookup plus a prefix check with `path.Join` collapsing `../` first |
| A revoked link answers identically to one that never existed | Not even the filename leaks. The public plane is a separate Caddy site block proxying only `/api/v1/s/*` and the SPA |

---

## Phase 3 — Sync engine ✅ 4/4, both sides

**Exit criterion:** two machines hold the same folder without you thinking about it;
a 4 GB file that changed by one block transfers one block; editing the same file on
both while offline produces a *conflict copy*, never a silent overwrite. **Met.**

| Slice | Contents | Status |
|---|---|---|
| 1 | Change journal: per-owner counter, trigger, `GET /changes`, retention | ✅ |
| 2 | Delta protocol: manifest, `chunks/have`, verified `PUT /chunks`, commit | ✅ |
| 3 | Go sync client: SQLite state, initial sync, fsnotify + rescan, push/pull | ✅ |
| 4 | Conflict resolution: lineage detection, conflict copies | ✅ |

| Rule | Detail |
|---|---|
| `seq` is a per-owner counter, not a `bigserial` | A bigserial assigns at insert time, so a transaction holding seq 9 can commit after one holding 10 and a reader at cursor 10 never sees 9. Bumped inside the writing transaction, seq order equals commit order. Cost: one user's concurrent writes serialise on their own counter |
| A journal row is an invalidation, not a snapshot | The client re-fetches current state, so a change immediately superseded is self-healing rather than stale. Populated by trigger, so moves that rewrite descendant paths and cascades no Go code names are still caught |
| `PUT /chunks/{hash}` never trusts its input | The server recomputes BLAKE3 and returns 400 on mismatch — a chunk stored under the wrong address corrupts every file that later dedups against it, across users |
| `GET /chunks/{hash}` is not an existence oracle | Scoped to users who already reference the chunk; 404 otherwise |
| The server owns the geometry | `CommitManifest` computes offsets and size from the chunks' own recorded sizes, and quota is charged at commit, so chunks uploaded but never committed cost the uploader nothing |
| The client is a separate Go module | It ships to laptops: no CGO, no pgx, no WebAuthn, and it cannot import the server's `internal` packages. An app password buys a confined device token at `POST /auth/token`, kept away from credential management |
| Two hashes per file in local state | The client's own whole-file BLAKE3 (judges local edits) and the server's reported hash (judges remote ones). Change detection gates on size+mtime; pull before push each pass |
| Conflicts are lineage, never clocks | Both the server's content hash and the local file's hash moved. No timestamp is consulted, so skew can neither fabricate nor mask a conflict. The local edit becomes `name (conflict from HOST DATE).ext`; all three routes to the hazard go through **one function** |

---

## Phase 4 — Intelligence, identity, hardening ✅ 6/6, both sides

**Exit criterion:** find a scanned receipt by a word printed on it, and a document by
what it is *about* — with no upload made slower and the plain file API intact when
the clever parts are switched off. **Met.**

| # | Slice | Status |
|---|---|---|
| 1 | Job queue (`jobs`, `SKIP LOCKED` claim, retry/backoff, reaper) + `pcworker` | ✅ |
| 2 | OCR / text extraction into `doc_text`, folded into trigram search | ✅ |
| 3 | Semantic search: embedding sidecar, packed float32, cosine KNN | ✅ |
| 3b | Auto-tagging: MIME category + curated vocabulary, reversible | ✅ |
| 4 | OIDC login alongside passkeys, opt-in | ✅ |
| 5 | Hardening pass | ✅ |
| — | Embedding sidecar reference implementation (`deploy/embed-sidecar`) | ✅ |
| — | Per-user API rate limiting | ✅ `8c7f792` — cost classes in `ratelimit_user.go`, applied in `requireAuth` |
| — | pgvector / HNSW index | ✅ optional accelerator: migration `00026`, `cloudctl embeddings`, HNSW per width |

**Two tiers, one queue — the architecture the hardware forced.** The always-on box
(7.2 GiB RAM, 4 cores, one spinner) owns state and never loads a model. Two RTX 4090
boxes are an *intermittent accelerator tier*: `pcworker` runs there, reaches Postgres
over the tailnet, and drains the same queue via `FOR UPDATE SKIP LOCKED` with no
schema change. Jobs simply wait when the GPUs are offline.

| # | ML rule |
|---|---|
| 1 | Intelligence is opt-in and out-of-band — never inline with an upload, never in the API process. Turning the worker off leaves exactly the Phase 3 system |
| 2 | Model choice follows the worker, not the API. The stored vector's dimension is a config of the chosen model, fixed per deployment |
| 3 | A remote worker pulls content over the authenticated API, never a mounted blob FS — the always-on box stays the only thing touching the blob store |
| 4 | Content never leaves your infrastructure. Local inference or the feature does not ship. No hosted API, ever. Training is out of scope |

| Component | Detail |
|---|---|
| Job queue | Idempotent **by content, not by job** (the worker re-reads the node's current version and keys results by content hash); a unique-pending index dedups per (kind, node); `attempts` + exponential `run_after` end in a dead letter rather than a loop pinning the one spare core; a reaper returns a crashed worker's job |
| Extraction | Shells out to tesseract — no cgo, and a crash in the C library takes the subprocess, not `pcworker`. Results content-addressed in `doc_text`, so a re-uploaded identical file is not re-OCR'd, and they feed the **existing** trigram search |
| Semantic search | The model runs in a Python sidecar called by both worker and API — an RPC is not a resident model, so rule 1 holds. Packed LE float32 is the stored form and the fallback ranking. Filtered by **model and dimension**, so a model retrained to a new width under the same name degrades to fewer results, never wrong ones. No sidecar → `503 semantic_unavailable`; lexical/OCR search untouched |
| pgvector, where it exists | Migration `00026` adds a `vec` column and creates the extension **if it can**, so stock Postgres migrates unchanged and a bare-machine restore still works — the property [runbook-restore.md](runbook-restore.md) depends on. `bytea` stays the source of truth and `vec` is a derived copy written alongside it. The read path ranks in SQL only when that copy is complete, checked against a partial index over the pending set; otherwise it falls back, so a half-backfilled table is slow rather than short of results. `cloudctl embeddings backfill` fills it and `… index` builds one **partial expression HNSW index per vector width**, refusing to build while any row is unconverted — an index over a half-filled column omits rows silently |
| Why the indexed path is also *more* correct | The exact scan bounds itself with `maxSemanticScan` and an `ORDER BY updated_at DESC` truncation, so past that many candidates it ranks the most **recent** vectors rather than the most **similar** ones — exactly when a corpus has grown enough to need ranking. Ordering by distance has no such cliff |
| The ACL filter did not move | It stays on the node rows, spliced from the same `Visibility` predicate. An ANN index that took its top-N from the whole corpus and filtered afterwards would hand a grantee an empty page whenever the nearest vectors belonged to someone else; `hnsw.iterative_scan` keeps the index walking until the page is filled from rows the caller may actually see |
| Auto-tagging | Deliberately the cheap kind — MIME category plus a small curated vocabulary, no classifier. Every tag names its `source`, re-tagging replaces only auto tags, and a removed tag is not re-applied: an auto-tagger that fights the user is worse than none |
| OIDC | Provisions its own users keyed by `(issuer, subject)` — the only identifier a provider promises stable — and never auto-links a passkey account by email, removing email-reassignment takeover. State, nonce and PKCE verifier ride in one short-lived single-use flow cookie; verification is delegated to `go-oidc`. OIDC users are non-admin; unconfigured → `404 oidc_disabled` |

### Hardening pass (slice 5) — reviewed, fixed, accepted

| Surface | Reviewed for | Verdict |
|---|---|---|
| Share plane (`/s/*`) | Revocation immediacy, password guessing, owner leak | ✅ row-based revoke, argon2id, rate limited, leak-free |
| Delta chunks (`PUT`/`GET /chunks`) | Address forgery, cross-user read | ✅ BLAKE3 recomputed; `GET` scoped to a referencing user |
| Device-token exchange (`/auth/token`) | Escalation from an app password | ✅ rate limited; device session confined from credential management |
| Job queue (enqueue on upload) | One user flooding the single worker | ✅ `OwnerQueueCap` + unique-pending index |
| OCR / extraction | A crafted file pinning the worker | ✅ 64 MiB cap, per-job timeout, PDF read bounded and panic-guarded |
| Extracted text | Stored-XSS / log injection | ✅ `doc_text` is used only for matching — never returned, never logged |
| Semantic search | Cross-space comparison, corpus blow-up | ✅ filtered by model **and** dimension; bounded by `maxSemanticScan` |
| Tags | Injection on display | ✅ control characters refused, length bounded |
| OIDC login | Forgery, CSRF, replay, takeover | ✅ go-oidc + single-use flow cookie + own users |

**Fixes landed:** pgx `v5.7.2 → v5.9.2`, clearing **GO-2026-5004** — a real SQL
injection via placeholder confusion reaching the whole data layer; a 2 MiB
`withBodyLimit` on every endpoint except those that legitimately stream (upload,
chunk `PUT`, resumable `PATCH`, WebDAV), previously an OOM lever on a 7 GiB box;
baseline security headers (`nosniff`, `X-Frame-Options: DENY`,
`Referrer-Policy: no-referrer`), the download path keeping its stricter
`Content-Security-Policy: sandbox`; tag input validation.
**Consciously accepted:** 🟠 one advisory in a
required-but-**uncalled** module (govulncheck confirms no code path reaches it).

---

## Phase 5 — Photos & media 🟠 server 8/8; video metadata open

**Exit criterion:** a person opens Photos, sees a date-ordered grid loading tiles
rather than originals, opens one full-screen, and groups a selection into an album
that moves nothing on disk. **Met.**

| # | Slice | Status |
|---|---|---|
| 1 | Media schema: content-addressed `media_meta` + `media_variant` (`00019`) | ✅ |
| 2 | Album schema: `albums` + `album_items` (`00020`) | ✅ |
| 3 | The `media` package: EXIF, `Analyze`, renderer, decode-bomb guards | ✅ |
| 4 | Metadata + variant store (`files/media.go`) | ✅ |
| 5 | `media` job handler, adapters, worker registration, enqueue on upload | ✅ |
| 6 | `?variant=thumb\|preview` on the content route | ✅ |
| 7 | `GET /media/timeline` sorted by capture time | ✅ |
| 8 | `/albums` CRUD + items + reorder | ✅ |
| 9 | Gallery, timeline, lightbox, album views in `web/` | ✅ |
| 10 | Add-to-album and pointer-drag reordering | ✅ `614c6e1` |
| 11 | Map view from EXIF GPS | ✅ `4875cb5` — `web/src/geo.ts`, no tile provider, so nothing phones home |
| — | Video metadata beyond "this is a video" | ❌ open item 9 |
| — | `cloudctl jobs reindex --kind=media` backfill | ✅ |
| — | `fsck`/GC account for variant bytes | ✅ the dangerous one — `--repair` would have deleted every thumbnail |

| Decision | Why |
|---|---|
| Everything derived is keyed by content hash, not node id | The same picture uploaded twice decodes once. On this hardware that is not a micro-optimisation: decoding a 24-megapixel JPEG twice costs seconds of CPU on a box with one spare core |
| `taken_at` is the point of the table | A timeline sorted by `created_at` is a timeline of your file transfers, not of your life. Nullable, with the fallback left to the client, so a UI can say "date unknown" instead of showing an import date as a capture date |
| Dimensions stored **as encoded**; GPS stored **exact and unrounded** | The owner already holds the original file with the same EXIF inside it, so blurring here would protect nothing and break the map view |
| Variants are a parameter, not a new route | `?variant=thumb\|preview\|original` reuses ranges, ETags, `Cache-Control`, disposition and the share plane, with no second byte-serving path to review. Two fixed sizes, because a free-form `?w=` means decoding on the request path of the always-on box |
| An unrendered variant is `404 variant_unavailable` | Never a silent fallback to the original — a gallery of 200 tiles quietly serving 12 MB originals looks like a network problem rather than a missing job |
| An album is not a folder | A node lives in one folder and many albums; adding does not move, removing does not delete. Folder-modelling means moving files (breaking every device's view of the tree) or copying them (breaking dedup and charging quota twice) |
| Ordering is replaced **wholesale** by `PATCH /albums/{id}/items` | A drag issuing N position updates is N chances to end up half-applied. Re-adding a node is a no-op, which is what makes a retry safe |
| Decoding is the hostile-input surface | The only place the system parses attacker-supplied binary formats in-process: `MaxInputBytes` 40 MiB (below the extractor's 64, because decoding is not streaming), **`MaxPixels` 80,000,000 checked from the header before anything decodes** (a 100-megapixel PNG can be a few hundred KB compressed and 400 MB decoded — file size is a poor proxy, pixel count is the honest one), an allowlist of jpeg/png/gif rather than an `image/*` prefix test (SVG is `image/*` and is not raster). A file that claims to be an image and is not is a *completed* job, not a retried one |
| Metadata written before variants render | Dimensions and capture time are what put a photo in the timeline at all; a missing thumbnail degrades one tile |

**The two halves drifted furthest apart here:** the front track finished a gallery
against endpoints that answered `404`, and nothing in the repository disagreed,
because the contract documented proposed endpoints beside shipped ones. Closed by
[openapi.yaml](openapi.yaml), generated from the real route table, plus the two
contract tests.

---

## Phase 6 — Native clients 🟠 packaging and tray left

**Exit criterion:** install a desktop app, sign in once, pick a folder, and see at a
glance whether files are up to date, pause on a hotspot, force a sync, find
conflicts. **Met except for the words "from the system tray"** — everything is
reachable from `pcsync watch` and the web app rather than an icon.

**Behind the API**

| Item | Status |
|---|---|
| `device_name` column (`00021`); a device *is* a device-kind session | ✅ |
| `GET /devices`, `PATCH /devices/{id}`, `DELETE /devices/{id}` | ✅ served, ✅ consumed by Settings |
| Device sessions forbidden from all five device routes (the escalation fix) | ✅ |
| `POST`/`DELETE /devices/{id}/push` — subscription storage | ✅ served — ❌ the one route in this repo with no client |
| VAPID public key for `PushManager.subscribe` · push delivery | ❌ open item 4 |

**In front of the API**

| # | Slice | Status |
|---|---|---|
| 1 | Local control socket: `/v1/status`, `/conflicts`, `/sync`, `/pause`, `/resume` | ✅ |
| 2 | Selective sync: excludes in both directions, persisted, `pcsync exclude` | ✅ |
| 3 | Tray presentation: platform-free `internal/tray` + `pcsync watch` | ✅ |
| 3b | Platform tray **icon + menu adapter** | ❌ open item 3 |
| 4 | Conflict list + dismiss, transfer tallies, `.pcsyncignore`, `pcsync doctor` | ✅ shipped, not in the original plan |
| 4b | Cross-platform release builds + `SHA256SUMS`, `pcsync version`, stale-client check | ✅ |
| 5 | Installable PWA: manifest, icon, offline app-shell service worker | ✅ |
| 6 | Offline file pinning (`pin.ts`, `Offline.tsx`, `pc-pinned` cache bucket) | 🟠 built and unit-tested; runtime SW behaviour wants on-device verification |
| 7 | Device management UI in Settings (name, platform, last seen, push state, revoke) | ✅ |
| 8 | Linux packages: `.deb`/`.rpm` for amd64+arm64 via nfpm | ✅ unsigned **by design** |
| 9 | Signed installers, apt/dnf repo, Homebrew/Scoop, `.msi`/`.pkg`, auto-update | ❌ open item 5 |
| 10 | PWA share target | ✅ `5a557a1` — manifest entry, `ShareTarget.tsx`, SW POST handler |

| Decision | Why |
|---|---|
| The control API is a **Unix domain socket** (0600), never a TCP port | No port means nothing on the network — not even loopback — can reach it, and on a shared machine another user cannot pause your sync. Slice 1 is not the tray icon; it is the contract between a GUI and the daemon |
| `Pause` stops the *automatic* cadences only | An explicit `SyncNow` still runs, so "paused" never means "stuck". None of it changes how reconciliation *decides* anything |
| The conflict log is a bounded in-memory ring | A "needs attention" hint, not an audit trail — the files themselves are the durable record |
| Selective sync is a **local** decision | An excluded subtree is never downloaded, a file created under one is never uploaded, and its absence never deletes it on the server. `pruneExcluded` reclaims a clean subtree but leaves one with unpushed edits on disk |
| A device is a session of kind `device` | A separate table would have to be kept in step with revocation; the reason "I lost my laptop" works is that revoking the session **is** revoking the device. `platform`/`app_version` are parsed at read time, and an unrecognised agent yields empty rather than a guess |
| The sharp edge, worth keeping | `requireAuth` confined device sessions away from credentials with a prefix test on `/api/v1/auth/`, and `/devices` is not under it — so as first written, one leaked app password could revoke every other device on the account. The check is now about what a path *does* rather than where it lives. Push is confined for the same reason |
| The revocation bug a test caught | Revoking a session is a soft delete, so `ON DELETE CASCADE` never fired and a revoked device stayed a push delivery target — still notified about a library it could no longer read. Both now happen in one statement |
| A shell-cache version bump preserves `pc-pinned` | An app update must never evict a user's offline files. Pinning is network-first for freshness and to revalidate auth, cache on failure |

---

## Phase 7 — Multi-user, sharing, RBAC, admin, quotas 🟠 one client gap

**Exit criterion:** two accounts share a folder in either direction, each seeing
exactly what they were given; an administrator provisions, quotas and disables
accounts without an SSH session; every access decision is answerable after the fact.
**Met.** The client views were built against the contract months before the server
half existed, degraded to "not available on this server yet", and **lit up
unchanged** when the handlers landed.

**Behind the API**

| # | Slice | Status |
|---|---|---|
| 1 | `grants` + `audit_log` (`00022`), `AccessFor`, path-prefix inheritance | ✅ 16 tests |
| 2 | Grant endpoints, `/shared`, `?include_shared=` on children/search/tags | ✅ 11 tests — 🟠 the query parameter has no caller |
| 3 | Admin users, sessions, audit endpoints | ✅ 8 tests |
| 4 | Editor writes: owner-charged quota, move and delete semantics | ✅ 7 tests |
| 5 | Per-user API rate limiting | ✅ `8c7f792` — keyed by user id, so one heavy caller is slowed without touching anybody else |
| — | Last-admin guard | 🟠 open item 14 |

**In front of the API**

| Item | Status |
|---|---|
| Share-with-people panel ([PeopleShare.tsx](../web/src/PeopleShare.tsx)) | ✅ direct grants only — an inherited grant belongs to the ancestor carrying it |
| Shared-with-me view ([SharedWithMe.tsx](../web/src/SharedWithMe.tsx)) | ✅ |
| Admin console: users, quotas, audit log ([Admin.tsx](../web/src/Admin.tsx)) | ✅ |
| Per-user session revocation · quota / usage indicators (`GET /usage`) | ✅ |
| Browsing **into** a granted folder (`?include_shared=true`) | ❌ open item 1 — `api.ts` can send it; no view opts in |

| Decision | Why |
|---|---|
| Widening is **opt-in per request** | Every query filtered `owner_id = $me`, and "files I can see but do not own" changes what existing endpoints *mean* without changing their shape — the worst kind of break, because nothing errors and no client fails to parse. Anything other than an explicit `true` reads as false, so a typo widens nothing |
| `GET /nodes/{id}` and `.../content` are deliberately not gated | Both name a single node the caller explicitly asked for, and gating them would make `/shared` incoherent — a viewer grant that cannot read the bytes is not a share |
| Inheritance is derived, not stored | A folder share must cover files that do not exist yet, and expanding grants into rows would need maintaining on every create, move, rename and restore |
| **`starts_with`, not `LIKE`** — and a trailing separator | The pattern is built from a column, so Go-side escaping cannot reach it: an unescaped column turns every metacharacter in a folder *name* into a wildcard, and a grant on `100%_done` also matched `1009Xdone`. Bare prefixes make a grant on `/project` cover `/projectX` |
| One predicate (`VisibleNodes`) spliced everywhere | Children, search, semantic search and tags share it — three slightly different wordings is how a file becomes readable through one endpoint and not another |
| The access decisions | Owner is checked first and never consults grants; two grants give the **union** (editor beats viewer); "no access" and "no such node" are the **same answer**, including for an unknown username, so probing cannot enumerate accounts; **only the owner may grant** (an editor re-sharing spreads access beyond what the owner can revoke); either party may revoke; `owner` is a real role but not a grantable one |
| An editor's writes land in the owner's tree and on the owner's quota | Anything else makes sharing a way to spend someone else's storage. An editor cannot move a shared file into their own tree, and an editor's delete goes to the **owner's** trash so the owner can restore it |
| Semantic search filters the NODE rows, never the vectors | Embeddings are content-addressed, so two users owning one document share a vector row by construction; filtering vectors would hide a document from someone entitled to it, or let one user's query surface another's through a similarity score. Tag counts are per caller, because a global count is an existence leak through a number |
| `DELETE /admin/users/{id}` disables and revokes; it does not delete | Deleting cascades a person's files away, and "remove this person's access" almost never means "destroy everything they uploaded". Disabling revokes every live session in the same call, because an account whose browser tab keeps working is not disabled |
| `quota_bytes` distinguishes absent from null | For a nullable column those are opposite instructions. Creating a user returns recovery codes once, reusing the recovery path rather than inventing invite tokens; `cloudctl` stays the strictly-more-powerful break-glass path |
| The audit log records authorisation-relevant events, not reads | A log that records everything is one nobody reads. An editor's write into a shared folder is logged; the same person's upload into their own folder is not. `actor_id` is `ON DELETE SET NULL` with the username denormalised beside it. The write is best-effort and detached from the request context: an unlogged grant is a gap in the record; a grant refused because the log was busy is a broken feature |

**Open risks:** an editor can exhaust the owner's quota (correct owner for the bytes;
nothing bounds how much an editor may add, and the remedy is revocation after the
fact), and **grants survive a move**, so an owner can silently change what a grant
covers by moving files into or out of a shared folder — visible in `GET /grants`, but
not announced.

---

## Phase 8 — Advanced intelligence 🟠 4/5 server slices

**Exit criterion:** ask a question of your own documents and get an answer that says
which documents it came from; find files like the one you are looking at; browse
photos by who is in them, correcting the machine when it is wrong. **Met.**

**The rule the whole phase obeys:** every endpoint here depends on a sidecar, and
every one degrades with a stable code rather than a 500 or a hang. The file API,
gallery, sync and search stay fully functional with all of it switched off.

**Behind the API**

| # | Slice | Status |
|---|---|---|
| 1 | `/nodes/{id}/similar` + the shared retrieval layer | ✅ 8 tests, ✅ consumed |
| 2 | `POST /chat`: retrieval, generation client, mandatory citations, degraded modes | ✅ 7 tests, ✅ consumed |
| 3 | Face schema (`00023`–`00025`), detector client, `faces` job, clustering, `/people` | ✅ 8 tests, ✅ consumed |
| 4 | Streaming answers (`stream: true`, SSE) | ✅ `bd59bf6` — citations first, `done` always sent, buffering defeated; 🟠 no browser consumer (open item 2) |
| 5 | Image-embedding similarity for photos | ❌ open item 6 |
| — | `cloudctl jobs reindex --kind=faces` | ✅ opt-in, deliberately outside `--kind=all` |

**In front of the API**

| Item | Status |
|---|---|
| Ask your library ([Ask.tsx](../web/src/Ask.tsx)) — answer + mandatory citations via `POST /chat` | ✅ non-streaming |
| People / faces browser, open a cluster, name it ([People.tsx](../web/src/People.tsx)) | ✅ |
| Face correction in the lightbox — "who's here", reassign, detach | ✅ |
| "Find similar" strip in the photo viewer | ✅ |
| Feedback controls that feed labels back | ❌ open item 10 |

**Three sidecars, not one** — separate because their resource profiles differ and a
deployment may want one and not the others; folding detection into the media job
would tie thumbnailing, which every deployment wants, to a sidecar most will not run.

| Sidecar | Env | Used by | Go client | Reference image |
|---|---|---|---|---|
| Embedding | `PC_EMBED_URL` | semantic search, similarity, chat retrieval | ✅ | ✅ `deploy/embed-sidecar` |
| Generation | `PC_GENERATE_URL`, `PC_GENERATE_MODEL` | `POST /chat` written answers | ✅ | ✅ `deploy/generate-sidecar` |
| Detection | `PC_DETECT_URL`, `PC_DETECT_MODEL`, `PC_DETECT_DIM` | the `faces` job | ✅ | ✅ `deploy/detect-sidecar` |

Config rejects `PC_GENERATE_URL` without `PC_EMBED_URL`: a generator with nothing to
retrieve would answer every question from nothing at all.

| Decision | Why |
|---|---|
| Similarity and retrieval are one mechanism with a different source vector | Exactly **one place** where the ACL filter meets the vector store — two nearly identical scans drift, and for an ACL filter drift means a leak |
| `/similar` requires read on the source | Otherwise any node id becomes a probe whose scores leak the shape of a private document |
| Similar if **any** passage is close to **any** of the source's | Averaging lets one long unrelated section drown a genuine match. A file is excluded from its own results |
| An unindexed file is `404 not_indexed` | Empty would claim "nothing resembles this"; `503` would tell the client to retry a feature that is working fine |
| Passage text is not stored beside the vector | `doc_text` holds it content-addressed and `ChunkText` is deterministic, so it is recovered after ranking for the handful of chunks that survived |
| No generator → `200` with the passages and `answer_unavailable: "generation_disabled"` | Surfacing the sources is the trustworthy half of RAG and useful alone |
| Citations are mandatory, and the stream sends them **first** | The generator is handed passage *text*, because an answer grounded in filenames alone would be invention. Nothing retrieved means the generator is not called at all. Retrieval reaches only what the caller could already open, so chat cannot read around a permission |
| The prompt lives in the sidecar | Rebuilding the API to change a sentence of English is the wrong iteration loop |
| `scope.node_ids` / `scope.tags` are **not accepted** | A scope field that parses and silently does nothing is worse than an absent one |
| Faces are per-owner and **not** content-addressed | A document's vector describes its bytes and may be shared between two owners; a "people" graph describes who someone knows, so two users owning the same photograph must not share clusters, or naming a face in your library would name it in a stranger's |
| Clustering is incremental and greedy (`FaceMatchThreshold` 0.72) | A library grows one upload at a time; a global re-partition would either run constantly or keep renaming clusters somebody has already named |
| The threshold errs toward **splitting** | Merging two people reads as the feature being wrong about who someone is; scattering one person is one click to fix — which is why merge and reassign are part of the design |
| A cluster is unnamed until a person names it | The system never guesses an identity. Forgetting a cluster keeps the detections (`ON DELETE SET NULL`); reassigning to nothing detaches without deleting; bounding boxes are fractions, not pixels; a photo with **no** faces is still recorded as looked-at, or every faceless photo is re-detected forever |

**Risks carried:** the clustering scan is bounded (`MaxFaceScan`) but not indexed —
O(faces × clusters), fine at thousands, and still on the packed-bytes path that
document embeddings have now left — `faces.vector` and `people.centroid` are the
obvious next users of migration `00026`'s column;
greedy clustering is order-dependent, so it is not reproducible; citations make a
wrong answer *checkable*, not right; and **face detection is the most
privacy-sensitive thing this server does** — an operator enabling it should know they
are building a biometric index of everyone photographed by the people using it.

---

## Phase 9 — Scale & resilience 🟠 2.5/5, deliberately

| # | Slice | Status |
|---|---|---|
| 1 | `GET /admin/storage` from the same collectors the alerts read | ✅ 5 parser tests, ✅ consumed by the admin Storage tab |
| 2 | Quota enforcement end to end, including owner-charged editor writes | ✅ 6 tests, ✅ surfaced in the UI |
| 3 | Object-storage cold tier + tiering policy | ❌ open item 7 |
| 4 | DR automation / drills as code | 🟠 open item 8 — both drills automated; recovery deliberately manual |
| 5 | Billing hooks | ❌ open item 11 |

| Decision | Why |
|---|---|
| `/admin/storage` reads the same sources the alerts already use | The zpool textfile collector, restic's success timestamp, the jobs table — not a second notion of health. Two notions disagree eventually, and then nobody knows which to believe at the moment it matters |
| The API never shells out to `zpool` or `restic` | The console and Grafana look at the same numbers by construction. A hand-rolled parser, because a dependency to read four metric names this repository itself writes is a dependency for nothing |
| Never scrubbed is not the same as failed | `last_scrub_clean` is absent when a pool has never been scrubbed and `false` only when a scrub found errors — collapsing them reports a brand-new pool as damaged |
| Stale report ≠ healthy pool · no collector ≠ empty pool · accounted bytes ≠ capacity | `collected_at`, `collector.available`/`collector.path`, and separate accounting. A malformed line costs one metric, not the report |
| Quota refuses with `507 quota_exceeded` | A storage condition WebDAV clients already read as "stop, you are full". Trashed bytes still count because they really are on disk; purging frees them; clearing a quota restores unlimited; an editor spends the **owner's** quota |

**What the cold tier would take**, sketched so the next person does not start from
nothing: a second `blob.Store` over an S3-compatible API behind the interface that
already exists; a `tier` column on `blobs` and `chunks` plus a policy job demoting by
age and access recency; a read path that transparently promotes on access, with a
restore-before-read latency contract the download handler can express; and `fsck`
taught about a third location. That last point is the reason not to rush it — it is
the trap this codebase has already fallen into twice (chunks, then media variants),
both times leaving `--repair` willing to delete live data. Content "moved to cold" by
code that cannot reliably read it back is content that is gone, silently, for the
files least recently touched.

---

## By design — tradeoffs, not defects

These appear as disadvantages and are real, but each is a chosen position. Listing
them as bugs invites "fixing" them into a worse system.

| Position | Reason |
|---|---|
| No password/passphrase recovery | Losing the ZFS passphrase or the restic repo password is unrecoverable *by construction* — the encryption has no backdoor, which is the point. Mitigation is procedural (a manager **and** paper); passkey users get recovery codes. A recoverable master key would be recoverable by an attacker |
| No high availability | One server cannot have HA; the design optimises durability plus bounded recovery (snapshots + tested offsite restore). Chasing HA across two home machines buys complexity, not uptime |
| Tailscale-only, no public login | Zero public attack surface is a feature; the public plane is deliberately limited to share links |
| Two privileged containers (cadvisor, smartctl_exporter) | Documented, deliberate exceptions for per-container metrics and SMART disk health |
| Setup complexity (ZFS/Docker/Tailscale) | Inherent to a self-hosted, own-your-data system. It is not a "download an app" product and does not pretend to be |
| Unsigned Linux packages | A locally installed `.deb`/`.rpm` needs no signature; only a *repository* does |
| Face detection off unless a detector is configured, and `reindex --kind=faces` outside `--kind=all` | Queueing a job per photo on a server with no detector fills the dead-letter queue instead of doing anything |
| The map view ships no tile provider | An offline-capable PWA and a provider that phones home for every tile are in tension; picking a provider is picking what leaks, so GPS renders locally instead |

## Known limits worth stating

| Limit | Detail |
|---|---|
| 🟠 The chunk-existence oracle is closed at the cost of cross-user transfer dedup | `POST /chunks/have` answers from the caller's own chunks only, so a stranger is told to upload content the server already holds. Storage dedup is unaffected (`PutKeyed` no-ops for an existing key); the transfer is paid twice. The global answer was a truthful yes/no about whether any given content exists, for anyone who could guess the bytes — and bandwidth is the cheaper thing to spend |
| 🟠 Quota counts logical, deduplicated bytes, not blocks on disk | It is the number a person can predict from what they uploaded; actual disk depends on compression and on content shared with other accounts, and charging someone for a chunk they share with a stranger is not explicable. Consequence: the sum of every account's usage does not equal the pool's used bytes, and should not be expected to |
| 🟠 Clustering is greedy and order-dependent | A person photographed over years may end up in several clusters; merge and reassign are the correction path, and corrections are permanent (`faces.dismissed_at`, migration `00024`) |
| 🟠 Rate limiting is in-process | Both limiters — the per-IP auth one and the per-user one — hold their buckets in memory. Correct for a single node. If the API is ever replicated they have to move to shared state; until then an external store adds a dependency to the auth path for a property that does not exist here |
| 🟠 The pgvector copy doubles what a vector costs on disk | `vec` is a second copy of every embedding, so the table roughly doubles where the extension is in use. Kept rather than dropping the `bytea` because the packed form is the one that survives a restore onto a machine with no pgvector, and it is what the fallback ranks on. Storage is the cheaper thing to spend than portability |
| 🟠 An unclassified route is limited at `costNormal` | Default-normal is deliberate: forgetting to classify a new route can only ever be too strict, never a hole. Getting it wrong shows up as a client that is being slowed and says so — a failure that reports itself |
| ✅ Integration test isolation — fixed | They used to share whatever database `PC_TEST_DATABASE_URL` named, which worked exactly once per database: a second run met the first run's rows and the failures looked like regressions. [internal/testdb](../server/internal/testdb/testdb.go) now creates a database per test binary and drops it afterwards, and CI runs the suite twice in a row against one server as the regression test |

---

## The seam: who owns what

Two developers, **Vivian** (in front of the API) and **Guru** (behind it), split **by
layer rather than by feature**, because the hard part of "two people, independent" is
not dividing features — it is making sure they almost never edit the same file.
Coordination reduces to *"is the endpoint in the contract yet?"* — an answer in a
file, not in a meeting.

| Directory | Owner | Notes |
|---|---|---|
| `server/internal/**`, `server/cmd/{api,pcworker,cloudctl}` | **Guru** | handlers, files, jobs, blob, CAS, auth, DB, worker |
| `deploy/*-sidecar/`, ML on the 4090s | **Guru** | Python inference, models |
| `deploy/**`, `scripts/**`, `docs/runbook-*` | **Guru** | compose, monitoring, ops |
| `web/**` | **Vivian** | React web app (the PWA lives here — `mobile/**` was never created) |
| `client/**` (`pcsync`) | **Vivian** | headless sync engine, separate Go module (`desktop/**` was never created; the platform-free tray half is `client/internal/tray`) |
| [api-contract.md](api-contract.md) + [openapi.yaml](openapi.yaml) | **shared** | the one shared artifact; new endpoints are **additive** and land here first |
| this document | shared | plan of record |

### The retrospective worth keeping

| Finding | Detail |
|---|---|
| Contract-first held completely | Every phase's endpoint shapes landed in the contract before the handlers, and the front track coded against them exactly. Phase 7's three UIs were written months early and lit up unchanged |
| The mock server was never built, and that was better | The front track used *graceful degradation* — each panel renders "not available on this server yet" when the endpoint 404s. A fallback ships and keeps earning its place; a mock is scaffolding that rots. Cost worth naming: a UI that degrades quietly is one nobody notices is unfinished |
| Contract tests arrived after the drift they existed to prevent | Phase 5 shipped a finished gallery against endpoints answering `404`, and nothing in the repository disagreed, because the contract documented proposed endpoints beside shipped ones |
| The asymmetry the split did not anticipate | By Phase 8 the behind-the-API track was a full phase ahead and thirteen route shapes were served with no consumer. None broken, just unreachable — but "both tracks move independently" turned out to mean one can finish a phase alone and the phase still is not done |
| The lesson | Layer ownership kept merge cost near zero, but it made *sequencing* invisible, and sequencing was the actual risk. The fix was not a process change; it was a test that fails when a route has no consumer and nobody has said why |

---

## The rule this document lives by

**Anything absent on purpose gets a line here, with its reason. If it is not here and
it is not built, that is a bug, not a decision.**

Two things are enforced mechanically rather than by this file: routes with no client
(`awaitingClient` in `contract_test.go`, which fails in both directions) and every
claim CI can execute. Everything else is a judgement, and a judgement can only be
written down.
