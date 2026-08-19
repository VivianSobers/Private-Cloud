# Private Cloud — the one status document

Every phase, every slice, one legend, one page. This replaces the nine phase
design documents, the roadmap split, the remaining-work register and the
deferred-work list, which said the same things in five places and disagreed
occasionally.

**How to use it:** read the [roll-up](#roll-up), then [what is not
done](#what-is-not-done--the-whole-open-list). Anything ✅ is built, wired end to
end and covered by tests. Anything 🟠 or ❌ is either the next thing to pick up or
a decision with its reasoning written beside it — those are marked **by design**,
and "fixing" one of them makes the system worse, so read the reason before
starting.

The only other documents in `docs/` are the ones this cannot replace: the
[Phase 0 operator checklist](phase-0-checklist.md) (things you do on the real
server with `sudo`), the [API contract](api-contract.md) and
[openapi.yaml](openapi.yaml) (the seam between the two tracks), the three
runbooks, [tailscale-setup.md](tailscale-setup.md) and
[custom-metrics.md](custom-metrics.md).

## Legend

| Mark | Means |
|---|---|
| ✅ | **Done** — built, wired end to end, covered by tests in this repo |
| 🟠 | **Partial** — one half exists, or it works with a stated limit; the row says which half |
| ❌ | **Not built** — no code. Either deliberately deferred (reason given) or blocked on something outside this repo |

A phase is ✅ only when the endpoint **and** the thing that consumes it both
exist. That rule is why the later phases get two columns.

## How the marks are derived

Read out of the merged tree (`origin/main` + `upstream/main`):

- the server's **route table** (`server/internal/httpapi/*.go`) — what actually answers
- **`awaitingClient`** in [contract_test.go](../server/internal/httpapi/contract_test.go) — routes served that no client calls, each with a reason, enforced by a test that fails in both directions (unconsumed *and* undeclared, or a declaration gone stale)
- **`web/src`** and **`client/`** — what a person can actually reach
- **`deploy/`**, **`scripts/`**, **`deploy/systemd/`** — what an operator gets

The counts behind the marks: **105 registered routes**, **1 route shape with no
client** (down from 13), **25 migrations**, **506 Go test functions across 73 test
files**, **20 web test cases**, and **one sidecar image in `deploy/` where three
are configurable**.

**This is a test run, not a reading of the code.** Everything marked ✅ was built
and executed: the Go suite (twice in a row against one database server, which is
itself the regression test for test isolation), the web suite, the alert-rule unit
tests, every dashboard query, and the ingress config in both TLS modes.
[.github/workflows/ci.yml](../.github/workflows/ci.yml) runs the same set on every
push, so the claim stays checked rather than being true once.

---

## Roll-up

| Phase | Scope | Behind the API | In front of it | Overall |
|---|---|---|---|---|
| 0 | Storage, network, monitoring, backups, runbooks | ✅ all of it is code, and CI proves the restore path | — | ✅ |
| 1 | MVP: auth, files, resumable upload, web UI, WebDAV, search | ✅ 7/7 slices | ✅ | ✅ |
| 2 | CAS engine, versioning, dedup, share links | ✅ 4/4 slices | ✅ | ✅ |
| 3 | Sync engine: journal, delta protocol, Go client, conflicts | ✅ 4/4 slices | ✅ `pcsync` | ✅ |
| 4 | OCR, semantic search, tagging, OIDC, hardening | ✅ 6/6 slices | ✅ | ✅ |
| 5 | Photos & media: EXIF, thumbnails, albums, timeline | ✅ 8/8 slices | 🟠 gallery, lightbox, albums ✅; **no map view**; reorder is buttons, not drag | 🟠 |
| 6 | Native clients: desktop tray, mobile/PWA | ✅ devices + push-subscription hook | 🟠 control socket, selective sync, `watch`, `doctor`, PWA, pinning, device UI ✅; **tray icon shell, signed installers, share target** ❌ | 🟠 |
| 7 | Multi-user, sharing, RBAC, admin, quotas | ✅ 4/4 slices | 🟠 sharing, admin, quotas ✅; **browsing into a granted folder** ❌ | 🟠 |
| 8 | Advanced intelligence: faces, similar files, RAG chat | 🟠 3/5 slices — streaming and image similarity ❌ | ✅ chat, people browser, find-similar, face correction | 🟠 |
| 9 | Scale & resilience: cold tier, DR automation, quotas | 🟠 storage health and quota ✅; cold tier, DR automation, billing ❌ | ✅ admin Storage tab | 🟠 |

**Phases 1–4 are finished on both sides. Phases 5–9 are finished behind the API,
and the front-of-API track has caught up on everything except four items** — a map
view, a shared-folder browser, the platform tray shell and signed installers. What
keeps 8 and 9 at 🟠 is *behind* the API, which is a reversal of where those phases
stood one merge ago.

**Nothing left is blocked on a decision.** The open client items are blocked on an
environment (a GUI to compile a tray against, code-signing keys) or are small and
unclaimed; the open server items are deliberate deferrals with the reasoning
written down.

---

## What is not done — the whole open list

Everything below is also in its phase section with the surrounding detail. This is
the pick-up list, ordered by how overdue each one is.

| # | Item | Phase | Mark | State, and what it needs |
|---|---|---|---|---|
| 1 | **Per-user API rate limiting** | 4→7→9 | ❌ | **The most overdue item in the project.** A per-IP limiter guards auth and search, and `OwnerQueueCap` bounds job enqueue; there is no per-user token bucket on the expensive endpoints. Deferring it was right at one trusted user. Phase 7 made a second account real and Phase 8 made one authenticated request able to spend GPU time on a sidecar, so the premise is gone. Small, unblocked, nobody's environment problem |
| 2 | **Browsing into a granted folder** | 7 | ❌ | Sequencing only. The server has supported `?include_shared=true` on children/search/tags since slice 2; `include_shared` appears nowhere in `web/src`, so a grantee reaches shared content through `/shared` and `/chat` but cannot open a shared *folder* inline |
| 3 | **Map view over photo GPS** | 5 | ❌ | `gps` is served unrounded and typed in `api.ts`; nothing renders it and no map library is a dependency. One real decision inside it: an offline-capable PWA and a tile provider that phones home for every tile are in tension, so picking a provider is picking what leaks |
| 4 | **Pointer-drag album reordering** | 5 | 🟠 | Add-to-album ✅ and wholesale `PATCH /albums/{id}/items` ✅; a person reorders with move-up/move-down buttons in a "Manage" mode. The endpoint contract — replace the order wholesale, never N per-item updates — was written for a drag and the buttons already satisfy it, so the remaining work is entirely in the browser |
| 5 | **Platform tray icon + menu adapter** | 6 | ❌ | Blocked on an **environment**, not a decision: needs a CGO system-tray library and a machine with a display. Everything it would render and every action it would fire is built and unit-tested in `client/internal/tray`; the adapter maps `tray.State` to an icon and wires menu items to the existing control client |
| 6 | **Push delivery (VAPID key + sender)** | 6 | ❌ | Both halves are **behind** the API, so this is not a missing UI: a PWA cannot call `PushManager.subscribe` without a VAPID public key the server does not publish, and nothing would deliver if it could. A client that registers nothing polls `GET /changes`, so push is a latency optimisation, never a correctness requirement |
| 7 | **Signed installers + auto-update** | 6 | 🟠 | Cross-built binaries + `SHA256SUMS` ✅ and unsigned Linux `.deb`/`.rpm` ✅ (unsigned **by design** — a local install needs no repo signature, only a *repository* does). ❌ A signed apt/dnf repo, Homebrew/Scoop, `.msi`/`.pkg` and an in-place updater need code-signing keys and per-OS tooling outside this repo. `pcsync doctor`/`version` flag a stale client meanwhile |
| 8 | **Streaming chat answers** | 8 | ❌ | `stream: true` is in the contract, unimplemented. Citations are computed from retrieved passages and are mandatory; an answer streaming ahead of its citations is, for the duration of the stream, exactly the unverifiable output this design refuses to produce. Worth having — needs the citation contract solved first |
| 9 | **Image-embedding similarity for photos** | 8 | ❌ | `/similar` works through the text-embedding space, so two photographs with no text have no neighbours. Needs a fourth model and a second vector space; most of the value is already delivered by face clustering and the timeline |
| 10 | **Generation + detection sidecar reference images** | 8 | ❌ | The Go clients and `PC_GENERATE_*` / `PC_DETECT_*` config exist and the wire shapes are tested; nothing in `deploy/` serves `POST /generate` or `POST /detect`. **By design** — a generator is a multi-gigabyte decision about quality, latency and VRAM, a detector is a decision about which biometric model an operator will run. Consequence: on a stock install `/chat` returns citations without prose and the people browser is correctly empty |
| 11 | **Object-storage cold tier** | 9 | ❌ | Not started, and honest about it: `GET /admin/storage` reports `tiering.enabled: false` rather than a cold tier holding zero bytes. **By design** until the preconditions are met — see [the sketch in Phase 9](#phase-9--scale--resilience) |
| 12 | **DR automation** | 9 | 🟠 | The **drill** is automated (`scripts/restore-drill.sh`, monthly timer, three alerts, run by CI against a real pool). The **recovery procedures** are still manual and deliberately so: automating a restore means automating something whose failure mode is overwriting good data with old data, under time pressure, with no second chance |
| 13 | **Video metadata beyond "this is a video"** | 5 | ❌ | `analyzeVideo` records the kind and nothing else — no duration, dimensions, rotation or thumbnail. Those live in MP4/MKV boxes needing a real demuxer; both honest options (cgo ffmpeg, or shelling out) belong behind the same "is this deployment set up for it" switch OCR sits behind. Consequence: a video sorts by upload time and has no thumbnail |
| 14 | **Feedback controls that feed labels back** | 8 | ❌ | Unbuilt on both sides and needs a decision first: a face correction is already permanent (`faces.dismissed_at`), so "feedback" here means labels that retrain a model, which Phase 4 put out of scope |
| 15 | **PWA share target** | 6 | ❌ | Small, unclaimed |
| 16 | **Billing hooks** | 9 | ❌ | Not started. There is no second tenant; quota exists and is enforced, and the thing billing would attach to is one person's disk |
| 17 | **Encrypted pool auto-unlock** | 0 | 🟠 | **Decided, deliberately not enabled.** The unit exists and its header documents what each keyfile location costs; `scripts/zfs-unlock.sh` refuses a key on the root filesystem, because storing the key beside the ciphertext it protects is not a weaker setup, it is no setup. Cost of leaving it off: a remote reboot needs a console |
| 18 | **`restore-test.sh` against *your* pool** | 0 | 🟠 | Operator gate. CI proves the restore path works against a loopback pool on every push; only you can prove *your* disks do |
| 19 | **pgvector / HNSW index** | 4, 8 | ❌ | Deferred by design — the exact cosine scan is correct at this corpus size, bounded by `maxSemanticScan`, and the query layer adopts pgvector with no schema change. Face clustering now wants the same upgrade for the same reason |
| 20 | **Last-admin guard refusal test** | 7 | 🟠 | The guard exists and its succeeding path is tested; its **refusal** path has no integration test, because the condition is a global property of the `users` table that every fixture's second admin defeats. Recorded, not papered over |

### Served, with no client

One route shape, down from thirteen. The authoritative list is `awaitingClient` in
[contract_test.go](../server/internal/httpapi/contract_test.go); a test fails both
when a route is unconsumed *and* undeclared, and when a declaration goes stale — so
the twelve that shipped had to be deleted from it as their UIs landed.

| Route | Waiting on |
|---|---|
| `POST`/`DELETE /devices/{id}/push` | a VAPID public key the server does not publish, and a sender that does not exist — **both behind the API**, so this is not waiting on a UI |

`?include_shared=true` is in the same position and is *not* covered by that test,
because it is a query parameter rather than a route: supported on children, search
and tags, and sent by nothing.

---

## Phase 0 — Foundation

✅ **Complete.** Everything is defined as code and committed. What is unticked
needs `sudo` on the real server, which no checkout can verify for you — the
step-by-step procedure is [phase-0-checklist.md](phase-0-checklist.md).

**Exit criterion:** ZFS pool healthy · Tailscale connected · Docker stack up ·
Grafana showing data · ntfy test alert received on your phone · restore test
executed successfully.

| Item | Status |
|---|---|
| ZFS pool + dataset layout (`scripts/zfs-setup.sh`) | ✅ |
| Snapshot ladder (`scripts/sanoid-setup.sh`, `deploy/sanoid/`) | ✅ |
| Tailscale-only plane, zero forwarded ports (`deploy/caddy/Caddyfile`) | ✅ |
| Docker stack: Postgres, Caddy, Prometheus, Grafana, Alertmanager, ntfy, exporters | ✅ |
| Nightly encrypted restic backup + freshness metric | ✅ |
| Pool-health textfile collector + systemd timer | ✅ |
| Alert rules, with rule tests (`alerts.yml`, `alerts_test.yml`) | ✅ 38 rules |
| Runbooks: [restore](runbook-restore.md), [disaster recovery](runbook-disaster-recovery.md), [worker](runbook-worker.md) | ✅ |
| Restore drill, automated (`scripts/restore-drill.sh` + monthly timer + 3 alerts) | ✅ CI runs it against a real ZFS pool on loopback vdevs every push |
| `scripts/restore-test.sh` executed against **your** pool | 🟠 operator gate — CI proves the path; only you can prove your disks |
| Grafana dashboards committed and self-provisioning | ✅ five, including a purpose-built Private Cloud overview; CI parses all 350 queries |
| Images pinned to digests (+ Renovate to move them) | ✅ all ten third-party images; CI fails on an unpinned one |
| Real TLS via `tailscale cert` instead of `tls internal` | ✅ script + weekly timer; CI validates the Caddyfile in both states |
| UPS + NUT | ✅ `deploy/host/nut/`, power events to the same ntfy topics |
| Unattended security upgrades | ✅ `deploy/host/apt/`, security origins only, ZFS/kernel blacklisted |
| pgBackRest point-in-time recovery | ✅ RPO 24h → one WAL segment; recovery in [runbook-restore.md](runbook-restore.md) §4c |
| Encrypted pool auto-unlock | 🟠 **decided, not enabled** — see open item 17 |
| Backup-freshness metric · pool-health metric | ✅ both landed as follow-ups |
| Host-level install in one command (`scripts/host-setup.sh --all` / `--check`) | ✅ |
| CI ([ci.yml](../.github/workflows/ci.yml)) | ✅ build, vet, race, govulncheck, contract, dashboards, alert-rule tests, Caddy, compose, shellcheck, restore drill |

**Why a pinned digest gets Renovate:** a pinned digest with nothing to update it
is a security problem wearing a reproducibility costume.

**Why the UPS is not about uptime:** ZFS survives one power cut by design; what it
survives badly is the second and third during the resilver after the first.

---

## Phase 1 — MVP

✅ **Complete — 7/7 slices, both sides.**

**Exit criterion:** you stop using Google Drive for manual file workflows — not
"the API works", but you keep real files here because it is the more convenient
option.

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

**The decisions that are still load-bearing:**

- **The versioned schema was built a phase before it was needed.** Phase 1 wrote
  one version per file, so Phase 2's CAS migration was "change what
  `file_versions` points at" rather than a schema rewrite over live data. Same for
  `blob.Store` — `Put/Get/Has/Delete`, filesystem-backed, swapped for FastCDC +
  BLAKE3 without touching the API.
- **`name_fold`.** Linux is case-sensitive, macOS and Windows are not; a Mac
  WebDAV client will happily create `Photos` beside `photos`. Display name *and* a
  folded form, `UNIQUE (parent_id, name_fold) WHERE trashed_at IS NULL`, from the
  first migration — retrofitting it after clients exist means reconciling real
  collisions.
- **`path` alongside `parent_id`.** The adjacency list is truth; the materialised
  path is a cache for prefix queries, and it is what makes Phase 7's inherited
  ACLs a prefix test rather than a row per descendant. Cost: a folder rename
  rewrites the subtree's paths.
- **Write order is blob first, then the DB row.** A crash between them leaves an
  orphaned blob (harmless, GC-able) rather than a row pointing at bytes that do not
  exist (corruption the user sees). Never the reverse. `fsck` exists from slice 3
  to prove the invariant holds.
- **Passkey lockout is real, so recovery codes and `cloudctl user reset-auth` are
  non-negotiable.** There is no forgot-password email, no second admin, no service
  to ask. Root on the box is already total access, so the CLI escape hatch weakens
  nothing.
- **Auth is slice 2, before files, deliberately** — retrofitting authorization into
  handlers that assumed one user is miserable and error-prone.
- **WebDAV at slice 6** made every OS file manager a working client months before
  the sync engine existed; it is also why app passwords had to exist.
- **Search ranks, it does not just filter.** A bare `LIKE` over a trigram index
  returns matches in table order, useless past a screenful; the shipped query ranks
  exact → prefix → similarity → recency.
- Stack revisions from the original plan, recorded: **stdlib `net/http`** rather
  than chi (Go 1.22 patterns; one less dependency in the auth path) and **no job
  queue in Phase 1** (adding a queue before there is a job is infrastructure for
  its own sake — it arrives in Phase 4 with a reason).

---

## Phase 2 — Storage engine

✅ **Complete — 4/4 slices, both sides.**

**Exit criterion:** you can recover a file you overwrote three weeks ago, send
someone a link to a folder without giving them an account, and the pool holds
noticeably more than the sum of the bytes you uploaded.

| Slice | Contents | Status |
|---|---|---|
| 1a | Chunk store: FastCDC + BLAKE3 + zstd behind `blob.Store`; both formats coexist | ✅ `internal/cas` |
| 1b | All three write paths (direct, resumable, WebDAV) route through the chunker | ✅ |
| 2 | Chunk GC, refcount recompute, CAS-aware `fsck`, blob migration, dedup stats | ✅ |
| 3 | Version history: list, restore, retention, UI | ✅ |
| 4 | Share links: public plane, tokens, password, expiry, cap, revocation, UI | ✅ |

**Chunking parameters (they are protocol now, not an implementation detail —
Phase 3's client re-declares them):** FastCDC, min 2 KiB / target 16 KiB / max
64 KiB, BLAKE3-256, zstd level 3. Files below the minimum stay whole-file blobs
permanently; already-compressed content skips zstd, detected by MIME plus a cheap
entropy check and recorded per chunk so the reader never guesses.

**Refcounting is the dangerous part.** One chunk is referenced by many manifests
across many users, so an undercount is unrecoverable data loss and an overcount is
wasted disk — not symmetric, and every choice resolves toward the overcount:
trigger-maintained, verified inside the deleting transaction, recomputable by
`fsck` which reports drift without acting on it.

**The traps this phase actually fell into, kept because they recur:**

- `fsck` had to understand chunks *before* anything wrote them. Chunks share the
  `ab/cd/hash` layout with blobs but are recorded in `chunks`, so a blob-only
  checker classifies every deduplicated byte as an orphan and `--repair` deletes
  it. `TestFsckDoesNotTreatChunksAsOrphans` keeps it that way. (Phase 5 hit the
  identical trap with media variants.)
- The refcount trigger fired on INSERT/DELETE only; migration `00008` taught it
  **UPDATE**, or the in-place blob→manifest switch would move the reference off
  the old blob without decrementing it and GC would leak every migrated blob
  forever. `TestMigrateDropsOldBlobToZeroThenGC` pins it.
- Blob migration is an **in-place UPDATE** of the version row, never a new
  version, so history is untouched and the API cannot tell a migrated file from
  one uploaded straight to CAS. A version whose bytes are already gone is
  **failed, never repointed**.
- GC ordering is manifests before chunks, so one pass reclaims a purged file all
  the way to its bytes.

**Versioning:** restore is an **append** — a new version pointing at the target's
existing content — never a deletion of the versions in between, so the rollback is
itself undoable and a 4 GB restore is one row and zero bytes. Retention prunes only
versions failing **both** tests (beyond `KeepVersions` AND older than
`VersionRetention`) and **never the head**, guarded by id rather than rank.

**Share links, the first thing reachable without an account:**

- A share is a **row, never a signed token**, because revocation must be immediate.
- The URL token is 256 bits, stored **hashed** (SHA-256 — it is full entropy),
  returned exactly once at creation.
- Password unlock keeps **no server session**: argon2id verification returns an
  HMAC over a per-share `unlock_key`, path-scoped so a proof for one share cannot
  open another and the cookie is never even transmitted elsewhere.
- The download cap is enforced **in the increment** (`UPDATE ... WHERE
  download_count < max_downloads`), so concurrent downloads racing the last slot
  cannot both win.
- Folder shares are confined twice over — owner-scoped lookup plus a prefix check
  with `path.Join` collapsing `../` first.
- A revoked link answers identically to one that never existed, leaking not even
  the filename; the public plane is a **separate Caddy site block** proxying only
  `/api/v1/s/*` and the SPA.

**Quota counts logical bytes** from here on, and deliberately does not match disk
— see [known limits](#known-limits-worth-stating).

---

## Phase 3 — Sync engine

✅ **Complete — 4/4 slices, both sides.** The daemon this phase built is what
Phase 6 wraps in a control surface.

**Exit criterion:** two machines hold the same folder without you thinking about
it; a 4 GB file that changed by one block transfers one block; editing the same
file on both while offline produces a *conflict copy*, never a silent overwrite.

| Slice | Contents | Status |
|---|---|---|
| 1 | Change journal: per-owner counter, trigger, `GET /changes`, retention | ✅ |
| 2 | Delta protocol: manifest, `chunks/have`, verified `PUT /chunks`, commit | ✅ |
| 3 | Go sync client: SQLite state, initial sync, fsnotify + rescan, push/pull | ✅ |
| 4 | Conflict resolution: lineage detection, conflict copies | ✅ |

**`seq` is a per-owner counter, not a `bigserial`.** A bigserial assigns numbers at
insert time, so a transaction holding seq 9 can commit *after* one holding seq 10
and a reader at cursor 10 never sees 9. A counter bumped inside the writing
transaction serialises assignment behind that row's lock, so seq order equals
commit order and the cursor cannot skip. The cost — one user's concurrent writes
serialise on their own counter — is the correct trade.

**The journal row is an invalidation, not a snapshot.** The client re-fetches the
node's current state, so a change immediately superseded is self-healing rather
than stale. Population is a **trigger**, for the same reason refcounts are: moves
rewrite descendant paths in one statement and cascades delete rows no Go code
names.

**`PUT /chunks/{hash}` is the one endpoint that must never trust its input.** The
server recomputes BLAKE3 and returns 400 on mismatch — a chunk stored under the
wrong address corrupts every file that later dedups against it, across users.
`GET /chunks/{hash}` is scoped to users who already reference the chunk, and
answers 404 otherwise, so it is not an existence oracle. The server owns the
geometry: `CommitManifest` computes offsets and size from the chunks' own recorded
sizes, and quota is charged at commit, so chunks uploaded but never committed cost
the uploader nothing.

**The client is a separate Go module** (`client/`, `pcsync`) — it ships to laptops,
so no CGO, no pgx, no WebAuthn, and it cannot import the server's `internal`
packages. The chunking parameters are re-declared to match, because a protocol is a
contract, not shared code. A headless client exchanges an app password for a
confined device token at `POST /auth/token`; that device session is kept away from
credential management, because an app password cannot mint another credential and a
token exchanged from one must not either.

**Two hashes per file** in the local state DB: the client's own whole-file BLAKE3
(the baseline a local edit is judged against) and the server's reported hash (the
baseline a remote change is judged against). Change detection gates on size+mtime
and re-hashes only when they move, so an untouched tree costs a stat per file.
**Pull before push** each pass, so the local scan pushes against a fresh baseline.

**Conflicts are detected by lineage, never by clocks** — the server's content hash
moved *and* the local file's hash moved. No timestamp is consulted, so clock skew
can neither fabricate nor mask a conflict. The local edit becomes
`name (conflict from HOST DATE).ext` and is pushed as its own file while the
server's version keeps the name. A delete seen through the journal, a delete found
during a full reconcile, and a both-sides edit all go through **one function**, so
the three routes to the same hazard cannot drift apart.

---

## Phase 4 — Intelligence, identity, hardening

✅ **Complete — 6/6 slices, both sides.** Two things this phase named are still ❌:
per-user rate limiting (open item 1) and pgvector (open item 19).

**Exit criterion:** you find a scanned receipt by a word printed on it, and a
document by what it is *about* — and none of it made the box slower to upload a
file or unable to serve the plain file API with the clever parts switched off.

| Slice | Contents | Status |
|---|---|---|
| 1 | Job queue (`jobs`, `SKIP LOCKED` claim, retry/backoff, reaper) + `pcworker` | ✅ |
| 2 | OCR / text extraction into `doc_text`, folded into trigram search | ✅ |
| 3 | Semantic search: embedding sidecar, packed float32, cosine KNN | ✅ |
| 3b | Auto-tagging: MIME category + curated vocabulary, reversible | ✅ |
| 4 | OIDC login alongside passkeys, opt-in | ✅ |
| 5 | Hardening pass (see below) | ✅ |
| — | Embedding sidecar reference implementation (`deploy/embed-sidecar`) | ✅ |
| — | pgvector / HNSW index | ❌ by design at this scale |
| — | Per-user API rate limiting | ❌ open item 1 |

**Two tiers, one queue — the architecture the hardware forced.** The always-on box
is 7.2 GiB of RAM, 4 cores, one spinner, and it owns state: API, Postgres (with the
queue in it), blob store. It never loads a model. The two RTX 4090 boxes are an
*intermittent accelerator tier*: `pcworker` processes run there, reach Postgres
over the tailnet, and drain the same queue via `FOR UPDATE SKIP LOCKED` with no
schema change. Jobs simply wait when the GPUs are offline.

Four rules gate everything ML:

1. **Intelligence is opt-in and out-of-band** — never inline with an upload, never
   in the API process. Turning the worker off leaves exactly the Phase 3 system.
2. **Model choice follows the worker, not the API.** The stored vector's dimension
   is a config of the chosen model, fixed per deployment.
3. **A remote worker pulls content over the authenticated API, never a mounted
   blob FS** — the always-on box stays the only thing touching the blob store.
4. **Content never leaves your infrastructure.** Local inference or the feature
   does not ship. No hosted API, ever. Training is out of scope.

**Job queue details worth keeping:** claiming is the same in-transaction
`SKIP LOCKED` claim the journal's counter uses; jobs are **idempotent by content,
not by job** (the worker re-reads the node's current version and writes results
keyed by content hash); a unique-pending index dedups per (kind, node);
`attempts` + exponential `run_after` end in a dead-letter state rather than a loop
pinning the one spare core; a reaper returns a crashed worker's job to the queue.

**Extraction** shells out to tesseract as a subprocess — no cgo, and a crash in
the C library takes the subprocess, not `pcworker`. Results are content-addressed
in `doc_text`, so a re-uploaded identical file is not re-OCR'd, and they feed the
**existing** trigram search rather than a new query path.

**Semantic search** runs the model in a Python sidecar (`deploy/embed-sidecar`),
called by both the worker (documents) and the API (queries) — an RPC to a sidecar
is not a resident model, so rule 1 holds. Vectors are packed little-endian float32
scanned exactly in Go, no pgvector needed on stock Postgres. `SemanticSearch`
filters by **both model and dimension**, so a model retrained to a new width while
keeping its name leaves old vectors unreturned rather than mismatched into a zero
score — a mixed store degrades to fewer results, never wrong ones. With no sidecar:
`503 semantic_unavailable`, and lexical/OCR search is untouched.

**Auto-tagging is deliberately the cheap kind** — MIME category plus a small
curated keyword vocabulary, no classifier. Every tag names its `source`, re-tagging
replaces only auto tags and never a user's, and a removed tag is not re-applied: an
auto-tagger that fights the user is worse than none.

**OIDC provisions its own users, keyed by `(issuer, subject)`** — the only
identifier a provider promises stable — and never auto-links a passkey account by
email, which removes email-reassignment account takeover as a risk. State (CSRF),
nonce (replay) and the PKCE verifier ride in one short-lived, single-use flow
cookie. Verification is delegated to `go-oidc`, because a subtly wrong JWT check is
exactly how an SSO integration becomes an auth bypass. OIDC users are non-admin,
admin stays passkey-bootstrapped, and without config the endpoints are absent in
effect (`404 oidc_disabled`).

### The hardening pass (slice 5) — what was reviewed, fixed and accepted

| Surface | Reviewed for | Verdict |
|---|---|---|
| Share plane (`/s/*`) | Revocation immediacy, password guessing, owner leak | ✅ row-based revoke, argon2id, rate limited, leak-free |
| Delta chunks (`PUT`/`GET /chunks`) | Address forgery, cross-user read | ✅ BLAKE3 recomputed; `GET` scoped to a referencing user |
| Device-token exchange (`/auth/token`) | Escalation from an app password | ✅ rate limited; device session confined from credential management |
| Job queue (enqueue on upload) | One user flooding the single worker | ✅ `OwnerQueueCap` + unique-pending index |
| OCR / extraction | A crafted file pinning the worker | ✅ 64 MiB cap, per-job timeout, PDF read bounded and panic-guarded |
| Extracted text | Stored-XSS / log injection | ✅ `doc_text` is used only for matching — never returned by the API, never logged |
| Semantic search | Cross-space comparison, corpus blow-up | ✅ filtered by model **and** dimension; scan bounded by `maxSemanticScan` |
| Tags | Injection on display | ✅ control characters refused, length bounded |
| OIDC login | Forgery, CSRF, replay, takeover | ✅ go-oidc + single-use flow cookie + own users |

**Fixes landed:** pgx `v5.7.2 → v5.9.2`, clearing **GO-2026-5004**, a real
SQL-injection via placeholder confusion reaching the whole data layer; a 2 MiB
`withBodyLimit` on every endpoint except the ones that legitimately stream
(upload, chunk `PUT`, resumable `PATCH`, WebDAV) — previously an OOM lever on a
7 GiB box; baseline security headers (`nosniff`, `X-Frame-Options: DENY`,
`Referrer-Policy: no-referrer`, with the download path keeping its stricter
`Content-Security-Policy: sandbox`); tag input validation.

**Consciously accepted:** ❌ per-user rate limiting (open item 1); 🟠 one advisory
in a required-but-**uncalled** module, tracked because govulncheck confirms no code
path reaches it; ❌ pgvector.

---

## Phase 5 — Photos & media

🟠 **Server complete (8/8 slices); two front-of-API items open.** The exit
criterion is met and the phase is usable end to end — the photo viewer has since
grown Phase 8's face overlay and find-similar and Phase 6's offline pinning on top
of it.

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
| 10 | Add-to-album and reordering in `web/` | 🟠 open item 4 — buttons, not a pointer drag |
| 11 | Map view from EXIF GPS | ❌ open item 3 |
| — | Video metadata beyond "this is a video" | ❌ open item 13 |
| — | `cloudctl jobs reindex --kind=media` backfill | ✅ |
| — | `fsck`/GC account for variant bytes | ✅ the dangerous one — `--repair` would have deleted every thumbnail |

**This is the phase whose two halves drifted furthest apart**, and it is worth
knowing why: the front track finished a gallery against endpoints that answered
`404`, and nothing in the repository disagreed with anything else, because the
contract documents proposed endpoints beside shipped ones. That gap is closed by
[openapi.yaml](openapi.yaml), generated from the real route table, plus the two
contract tests.

**Everything derived is content-addressed**, keyed by content hash rather than node
id — the same choice `doc_text` and `doc_embedding` made. A photo's dimensions,
capture time and thumbnail are properties of the *bytes*, so the same picture
uploaded twice is decoded once. On this hardware that is not a micro-optimisation:
decoding a 24-megapixel JPEG twice costs seconds of CPU on a box with one spare
core.

**`taken_at` is the point of the table** — when the shutter fired, not when the
file was uploaded. A timeline sorted by `created_at` is a timeline of your file
transfers, not of your life. It is nullable and the fallback is left to the
client, so a UI can show "date unknown" instead of confidently displaying an
import date as a capture date. Dimensions are stored **as encoded**, before
orientation is applied. GPS is stored **exact and unrounded** — the owner already
holds the original file with the same EXIF inside it, so blurring here would
protect nothing and break a map view.

**Variants are a parameter, not a new route:**
`GET /nodes/{id}/content?variant=thumb|preview|original`. Reusing the content route
keeps ranges, ETags, `Cache-Control`, download disposition and the public share
plane working with no second byte-serving path to review. Two fixed sizes, not a
free-form `?w=`, because an arbitrary width means decoding on the request path of
the always-on box. An unrendered variant is an honest `404 variant_unavailable`,
never a silent fallback to the original — a gallery of 200 tiles quietly serving
12 MB originals looks like a network problem rather than a missing job.

**An album is not a folder.** A node lives in one folder and many albums; adding
does not move, removing does not delete. Modelling an album as a folder would mean
moving files (breaking every device's view of the tree) or copying them (breaking
dedup and charging quota twice). Ordering is replaced **wholesale** by
`PATCH /albums/{id}/items`, because a drag issuing N position updates is N chances
to end up half-applied; re-adding a node is a no-op, which is what makes a retry
safe.

**Decoding is the hostile-input surface** — the only place the system parses
attacker-supplied binary formats in-process: `MaxInputBytes` 40 MiB (lower than the
extractor's 64, because decoding is not streaming), **`MaxPixels` 80,000,000
checked from the header before anything decodes** (a 100-megapixel PNG can be a few
hundred KB of compressible black and 400 MB of decoded RGBA — file size is a poor
proxy for decode cost, pixel count is the honest one), an **allowlist** of
jpeg/png/gif rather than an `image/*` prefix test (SVG is `image/*` and is not a
raster image), and a file that claims to be an image and is not is a *completed*
job, not a retried one.

**Metadata is written before variants are rendered**, and a rendering failure is
logged rather than returned: dimensions and capture time are what put a photo in
the timeline at all, while a missing thumbnail degrades one tile.

---

## Phase 6 — Native clients

🟠 **Server side ✅ complete and consumed; the client is production-shaped with two
packaging items left, both blocked on tooling rather than a decision.**

**Exit criterion:** install a desktop app, sign in once, pick a folder, and see at
a glance whether files are up to date, pause on a hotspot, force a sync, find
conflicts. **Met except for the words "from the system tray"** — everything is
reachable from `pcsync watch` and the web app rather than an icon.

**Behind the API** — one column, one table, five routes, as the split predicted:

| Item | Status |
|---|---|
| `device_name` column (`00021`); a device *is* a device-kind session | ✅ |
| `GET /devices`, `PATCH /devices/{id}`, `DELETE /devices/{id}` | ✅ served, ✅ consumed by Settings |
| Device sessions forbidden from all five device routes (the escalation fix) | ✅ |
| `POST`/`DELETE /devices/{id}/push` — subscription storage | ✅ served — ❌ the one route in this repo with no client |
| VAPID public key for `PushManager.subscribe` · push delivery | ❌ open item 6 |

**In front of the API:**

| # | Slice | Status |
|---|---|---|
| 1 | Local control socket: `/v1/status`, `/conflicts`, `/sync`, `/pause`, `/resume` | ✅ |
| 2 | Selective sync: excludes in both directions, persisted, `pcsync exclude` | ✅ |
| 3 | Tray presentation: platform-free `internal/tray` + `pcsync watch` | ✅ |
| 3b | Platform tray **icon + menu adapter** | ❌ open item 5 |
| 4 | Conflict list + dismiss, transfer tallies, `.pcsyncignore`, `pcsync doctor` | ✅ shipped, not in the original plan |
| 4b | Cross-platform release builds + `SHA256SUMS`, `pcsync version`, version check in `doctor` | ✅ |
| 5 | Installable PWA: manifest, icon, offline app-shell service worker | ✅ |
| 6 | Offline file pinning (`pin.ts`, `Offline.tsx`, `pc-pinned` cache bucket) | 🟠 built and unit-tested; runtime SW behaviour wants on-device verification |
| 7 | Device management UI in Settings (name, platform, last seen, push state, revoke) | ✅ |
| 8 | Linux packages: `.deb`/`.rpm` for amd64+arm64 via nfpm | ✅ unsigned **by design** |
| 9 | Signed installers, apt/dnf repo, Homebrew/Scoop, `.msi`/`.pkg`, auto-update | ❌ open item 7 |
| 10 | PWA share target | ❌ open item 15 |

**Slice 1 is not the tray icon; it is the contract between a GUI and the daemon.**
The daemon serves a tiny HTTP API over a **Unix domain socket** (0600, in the state
directory), never a TCP port — no port means nothing on the network, not even
loopback, can reach it, and on a shared machine another user cannot pause your
sync. `Pause` stops the *automatic* cadences only; an explicit `SyncNow` still
runs, so "paused" never means "stuck". The conflict log is a bounded in-memory ring
— a "needs attention" hint, not an audit trail, because the files themselves are
the durable record. None of this changes how reconciliation *decides* anything.

**Selective sync is a local decision**: an excluded subtree is never downloaded, a
file created under one is never uploaded, and its absence never deletes it on the
server. `pruneExcluded` reclaims a newly-excluded clean subtree locally but leaves
one with unpushed edits on disk.

**A device is a session of kind `device`.** Nothing new represents one — a separate
table would have to be kept in step with revocation, and the whole reason "I lost
my laptop" works is that revoking the session **is** revoking the device.
`platform` and `app_version` are parsed from the stored agent at read time rather
than stored; an unrecognised agent yields empty rather than a guess, because a
wrong platform makes the list look authoritative when it is not.

**The sharp edge, worth keeping:** `requireAuth` confined device sessions away from
credentials with a prefix test on `/api/v1/auth/`, and `/devices` is not under it —
so as first written, a **single leaked app password could revoke every other device
on the account**. The check is now about what a path *does* rather than where it
lives. Push is confined for the same reason: a device registering an endpoint
against a *sibling's* id would redirect that device's notifications.

**And the revocation bug a test caught:** revoking a session is a **soft delete**
that stamps `revoked_at` and removes no row, so `ON DELETE CASCADE` never fired and
a revoked device stayed a push delivery target — still receiving notifications
about a library it could no longer read. Both now happen in one statement.

**Offline pinning** stores bytes in a dedicated Cache Storage bucket (`pc-pinned`)
that the service worker serves when the network is gone — network-first for
freshness and to revalidate auth, cache on failure. The shell-cache version bump
deliberately preserves the pin bucket, so an app update never evicts a user's
offline files.

---

## Phase 7 — Multi-user, sharing, RBAC, admin, quotas

🟠 **Server ✅ complete (4/4 slices) and consumed; one client gap.** The client
views were built against the contract months before the server half existed,
degraded to "not available on this server yet", and **lit up unchanged** when the
handlers landed — the strongest evidence in the repository that the layer split was
the right call.

**Exit criterion:** two accounts share a folder in either direction, each seeing
exactly what they were given; an administrator provisions, quotas and disables
accounts without an SSH session; every access decision is answerable after the fact.

**Behind the API:**

| # | Slice | Status |
|---|---|---|
| 1 | `grants` + `audit_log` (`00022`), `AccessFor`, path-prefix inheritance | ✅ 16 tests |
| 2 | Grant endpoints, `/shared`, `?include_shared=` on children/search/tags | ✅ 11 tests — 🟠 the query parameter has no caller |
| 3 | Admin users, sessions, audit endpoints | ✅ 8 tests |
| 4 | Editor writes: owner-charged quota, move and delete semantics | ✅ 7 tests |
| 5 | Per-user API rate limiting | ❌ open item 1 |
| — | Last-admin guard | 🟠 open item 20 |

**In front of the API:**

| Item | Status |
|---|---|
| Share-with-people panel ([PeopleShare.tsx](../web/src/PeopleShare.tsx)) | ✅ direct grants only — an inherited grant belongs to the ancestor carrying it |
| Shared-with-me view ([SharedWithMe.tsx](../web/src/SharedWithMe.tsx)) | ✅ |
| Admin console: users, quotas, audit log ([Admin.tsx](../web/src/Admin.tsx)) | ✅ |
| Per-user session revocation in the console | ✅ |
| Quota / usage indicators (`GET /usage`) | ✅ |
| Browsing **into** a granted folder (`?include_shared=true`) | ❌ open item 2 |

**The hazard this phase carried:** every query in the system filtered
`owner_id = $me`, and "files I can see but do not own" widens what the existing
endpoints *mean* without changing their shape — the worst kind of break, because
nothing errors and no client fails to parse. So the widening is **opt-in per
request**, and anything other than an explicit `true` reads as false, so a typo
widens nothing. `GET /nodes/{id}` and `.../content` are deliberately *not* gated:
both name a single node the caller explicitly asked for, and gating them would
make `/shared` incoherent — a viewer grant that cannot read the bytes is not a
share.

**Inheritance is derived, not stored** — a prefix test on the materialised path,
because a folder share must cover files that do not exist yet and expanding grants
into rows would need maintaining on every create, move, rename and restore. The
test uses **`starts_with`, not `LIKE`**, and a test is why: the pattern is built
from a column, so Go-side escaping cannot reach it, and an unescaped column turns
every metacharacter in a folder *name* into a wildcard — a grant on `100%_done`
also matched `1009Xdone`. The trailing separator matters for the same class of
reason: bare prefixes make a grant on `/project` cover `/projectX`.

**One predicate, spliced everywhere.** `VisibleNodes` is a single SQL constant used
by children, search, semantic search and tags — three slightly different wordings
is how a file becomes readable through one endpoint and not another.

**The access decisions, stated:** owner is checked first and never consults the
grants table; two grants give the **union** (editor beats viewer); "no access" and
"no such node" are the **same answer**, including for an unknown username, so
probing cannot enumerate accounts; **only the owner may grant** (an editor
re-sharing spreads access beyond what the owner can revoke); **either party may
revoke**; `owner` is a real role but not a grantable one.

**An editor's writes land in the owner's tree and on the owner's quota** — anything
else makes sharing a way to spend someone else's storage. So an editor cannot move
a shared file into their own tree, and an editor's delete goes to the **owner's**
trash so the owner can restore it.

**Semantic search filters the NODE rows, never the vectors.** Embeddings are
content-addressed, so two users owning the same document share one vector row by
construction; filtering vectors would hide a document from someone entitled to it
or — worse — let one user's query surface another's document through a similarity
score. **Tag counts are per caller**, because a global count is an existence leak
through a number.

**Admin, and what it deliberately cannot do:** `DELETE /admin/users/{id}` **disables
and revokes; it does not delete** — deleting cascades a person's files away, and
"remove this person's access" almost never means "destroy everything they
uploaded". Disabling revokes every live session in the same call, because an
account whose browser tab keeps working is not disabled. `quota_bytes`
distinguishes absent from null, because for a nullable column those are opposite
instructions. Creating a user returns recovery codes once, reusing the recovery
path rather than inventing invite tokens. `cloudctl` stays the strictly-more-
powerful break-glass path.

**The audit log records authorisation-relevant events, not reads** — a log that
records everything is one nobody reads. An editor's write into a shared folder is
logged; the same person's upload into their own folder is not. Entries carry the
`request_id`; `actor_id` is `ON DELETE SET NULL` with the username denormalised
beside it, because an entry that can no longer say who did the thing has lost the
only fact that made it worth keeping. The write is **best effort and detached from
the request context**: a grant that succeeded and was not logged is a gap in the
record; a grant refused because the log was busy is a broken feature.

**Open risks:** an editor can exhaust the owner's quota (correct owner for the
bytes; nothing bounds how much an editor may add, and the remedy is revocation
after the fact), and **grants survive a move**, so an owner can silently change
what a grant covers by moving files into or out of a shared folder — visible in
`GET /grants`, but not announced.

---

## Phase 8 — Advanced intelligence

🟠 **3/5 server slices ✅ and consumed; slices 4 and 5 ❌ not started, and two of
the three sidecars have no reference implementation.**

**Exit criterion:** ask a question of your own documents and get an answer that
says which documents it came from; find files like the one you are looking at;
browse photos by who is in them, correcting the machine when it is wrong.

**The rule the whole phase obeys:** every endpoint here depends on a sidecar, and
every one degrades with a stable code rather than a 500 or a hang. The file API,
gallery, sync and search stay fully functional with all of it switched off.

**Behind the API:**

| # | Slice | Status |
|---|---|---|
| 1 | `/nodes/{id}/similar` + the shared retrieval layer | ✅ 8 tests, ✅ consumed |
| 2 | `POST /chat`: retrieval, generation client, mandatory citations, degraded modes | ✅ 7 tests, ✅ consumed |
| 3 | Face schema (`00023`–`00025`), detector client, `faces` job, clustering, `/people` | ✅ 8 tests, ✅ consumed |
| 4 | Streaming answers (`stream: true`, SSE) | ❌ open item 8 |
| 5 | Image-embedding similarity for photos | ❌ open item 9 |
| — | Generation / detection sidecar reference implementations | ❌ open item 10 |
| — | `cloudctl jobs reindex --kind=faces` | ✅ opt-in, deliberately outside `--kind=all` |

**In front of the API:**

| Item | Status |
|---|---|
| Ask your library ([Ask.tsx](../web/src/Ask.tsx)) — answer + mandatory citations via `POST /chat` | ✅ |
| People / faces browser, open a cluster, name it ([People.tsx](../web/src/People.tsx)) | ✅ |
| Face correction in the lightbox — "who's here", reassign, detach | ✅ |
| "Find similar" strip in the photo viewer | ✅ |
| Feedback controls that feed labels back | ❌ open item 14 |

**Three sidecars, not one:**

| Sidecar | Env | Used by | Go client | Reference image |
|---|---|---|---|---|
| Embedding | `PC_EMBED_URL` | semantic search, similarity, chat retrieval | ✅ | ✅ `deploy/embed-sidecar` |
| Generation | `PC_GENERATE_URL`, `PC_GENERATE_MODEL` | `POST /chat` written answers | ✅ | ❌ |
| Detection | `PC_DETECT_URL`, `PC_DETECT_MODEL`, `PC_DETECT_DIM` | the `faces` job | ✅ | ❌ |

They are separate because their resource profiles differ and a deployment may want
one and not the others; folding detection into the media job would tie thumbnailing
— which every deployment wants — to a sidecar most will not run. Config rejects
`PC_GENERATE_URL` without `PC_EMBED_URL`: a generator with nothing to retrieve
would answer every question from nothing at all.

**Similarity and retrieval are one mechanism** with a different source vector, so
there is exactly **one place where the ACL filter meets the vector store** —
building them separately means two nearly identical scans that can drift, and for
an ACL filter drift means a leak. Reading the source is required for `/similar`,
or any node id becomes a probe whose scores leak the shape of a private document.
A file is excluded from its own results; a document is similar if **any** passage
is close to **any** of the source's, never on an average, because averaging lets
one long unrelated section drown a genuine match. An unindexed file is
`404 not_indexed` — empty would claim "nothing resembles this", `503` would tell
the client to retry a feature that is working fine. **Passage text is not stored
beside the vector**: `doc_text` holds it content-addressed and `ChunkText` is
deterministic, so it is recovered after ranking for the handful of chunks that
survived.

**Chat is two halves with different reliability.** With no generator, `POST /chat`
still answers `200` with the passages and `answer_unavailable:
"generation_disabled"` — surfacing the sources is the trustworthy half of RAG and
useful alone. **Citations are mandatory, not decorative**, and the generator is
handed passage *text*, because an answer grounded in filenames alone would be
invention. **Nothing retrieved means no answer** — the generator is not called at
all. Retrieval reaches only what the caller could already open, so chat cannot
become a way to read around a permission. The prompt lives in the sidecar, because
rebuilding the API to change a sentence of English is the wrong iteration loop.
`scope.node_ids` and `scope.tags` are **not accepted** — a scope field that parses
and silently does nothing is worse than an absent one.

**Faces are per-owner and not content-addressed**, unlike document embeddings, and
that difference is the point: a document's vector describes its bytes and may be
shared between two users owning the same file, but a "people" graph describes who
someone knows — two users owning the same photograph must not share clusters, or
naming a face in your library would name it in a stranger's.

Clustering is **incremental and greedy**: each face joins the nearest cluster above
`FaceMatchThreshold` (0.72) or starts one. Not a global re-partition, for an
operational reason — a library grows one upload at a time, and re-partitioning
would either run constantly or keep renaming clusters somebody has already named.
**A name is a promise.** The threshold errs toward **splitting**, because merging
two people reads as the feature being wrong about who someone is, while scattering
one person is one click to fix — which is why merge and reassign are part of the
design, not an afterthought. A cluster is **unnamed until a person names it**; the
system never guesses an identity. Forgetting a cluster keeps the detections
(`ON DELETE SET NULL`); reassigning to nothing detaches a face without deleting it;
bounding boxes are fractions, not pixels; a photo with **no** faces is still
recorded as looked-at, or every faceless photo is re-detected forever.

**Risks carried:** the clustering scan is bounded (`MaxFaceScan`) but not indexed —
O(faces × clusters), fine at thousands, wanting the same pgvector upgrade at a
hundred thousand; greedy clustering is order-dependent, so clustering is not
reproducible; a generated answer is only as good as its retrieval, and citations
make a wrong answer *checkable*, not right; and **face detection is the most
privacy-sensitive thing this server does** — an operator enabling it should know
they are building a biometric index of everyone photographed by the people using
their server.

---

## Phase 9 — Scale & resilience

🟠 **2/5 slices, deliberately.** The observability and quota halves are ✅ built and
surfaced; the cold tier, DR automation and billing are ❌.

| # | Slice | Status |
|---|---|---|
| 1 | `GET /admin/storage` from the same collectors the alerts read | ✅ 5 parser tests, ✅ consumed by the admin Storage tab |
| 2 | Quota enforcement end to end, including owner-charged editor writes | ✅ 6 tests, ✅ surfaced in the UI |
| 3 | Object-storage cold tier + tiering policy | ❌ open item 11 |
| 4 | DR automation / restore drills as code | 🟠 open item 12 — the drill is automated, the recovery procedures are deliberately manual |
| 5 | Billing hooks | ❌ open item 16 |

**`/admin/storage` reads the same sources the alerts already use** — the zpool
textfile collector, restic's success timestamp, the jobs table — rather than
inventing a second notion of health. Two notions disagree eventually, and then
nobody knows which to believe at the moment it matters. Nothing here runs
`zpool status` or `restic snapshots`; the API process does not shell out to storage
tooling, so the console and Grafana are looking at the same numbers by
construction. A hand-rolled parser, because pulling in a dependency to read four
metric names this repository itself writes would be a dependency for nothing.

**Distinctions the report is careful about:** never scrubbed is not the same as
failed (`last_scrub_clean` is absent when a pool has never been scrubbed, `false`
only when a scrub found errors — collapsing them reports a brand-new pool as
damaged); a stale report is not a healthy pool (`collected_at`); no collector is not
an empty pool (`collector.available`/`collector.path`); accounted bytes are not
pool capacity; a malformed line costs one metric, not the report.

**Quota enforcement** refuses with `507 quota_exceeded` (a storage condition WebDAV
clients already read as "stop, you are full"); trashed bytes still count because
they really are still on disk; purging frees them; clearing a quota restores
unlimited; and an editor writing into a shared folder spends the **owner's** quota.

**What the cold tier would take**, sketched so the next person does not start from
nothing: a second `blob.Store` over an S3-compatible API behind the interface that
already exists; a `tier` column on `blobs` and `chunks` plus a policy job demoting
by age and access recency; a read path that transparently promotes on access and a
**restore-before-read latency contract** the download handler can express; and
`fsck` taught about a third location. That last point is the reason not to rush it —
it is the trap this codebase has already fallen into twice (chunks, then media
variants), both times leaving `--repair` willing to delete live data. Content
"moved to cold" by code that cannot reliably read it back is content that is gone,
silently, for the files least recently touched.

---

## By design — tradeoffs, not defects

These appear as disadvantages and are real, but each is a chosen position. Listing
them as bugs invites "fixing" them into a worse system.

- **No password/passphrase recovery.** Losing the ZFS passphrase or the restic
  repo password is unrecoverable *by construction* — the encryption has no
  backdoor, which is the point. Mitigation is procedural (a manager **and**
  paper); passkey users get recovery codes. A recoverable master key would be a
  recoverable-by-an-attacker master key.
- **No high availability.** One server cannot have HA; the design optimises
  durability plus bounded recovery (snapshots + tested offsite restore). Chasing
  HA across two home machines buys complexity, not uptime.
- **Tailscale-only, no public login.** Zero public attack surface is a feature; the
  public plane is deliberately limited to share links.
- **Two privileged containers** (cadvisor, smartctl_exporter) — documented,
  deliberate exceptions for per-container metrics and SMART disk health.
- **Setup complexity (ZFS/Docker/Tailscale).** Inherent to a self-hosted,
  own-your-data system. It is not a "download an app" product and does not pretend
  to be.
- **Unsigned Linux packages.** A locally installed `.deb`/`.rpm` needs no
  signature; only a *repository* does.
- **Face detection is off unless a detector is configured**, and
  `reindex --kind=faces` is outside `--kind=all` — queueing a job per photo on a
  server with no detector fills the dead-letter queue instead of doing anything.

## Known limits worth stating

- 🟠 **The chunk-existence oracle is closed at the cost of cross-user transfer
  dedup.** `POST /chunks/have` answers from the caller's own chunks only, so a
  stranger is told to upload content the server already holds. Storage dedup is
  unaffected (`PutKeyed` is a no-op for an existing key); the transfer is paid
  twice. The global answer was a truthful yes/no about whether any given content
  exists on this server, for anyone who could guess the bytes — a real oracle, and
  bandwidth is the cheaper thing to spend.
- 🟠 **Quota counts logical bytes, deduplicated, not blocks on disk.** It is the
  number a person can predict from what they uploaded; actual disk depends on
  compression and on content shared with other accounts, and charging someone for a
  chunk they share with a stranger is not explicable. Consequence: the sum of every
  account's usage does not equal the pool's used bytes, and should not be expected
  to.
- 🟠 **Clustering is greedy and order-dependent.** A person photographed over years
  may end up in several clusters; merge and reassign are the correction path, and
  corrections are permanent (`faces.dismissed_at`, migration `00024`).
- 🟠 **Rate limiting is in-process.** Correct for a single node. If the API is ever
  replicated the limiter has to move to shared state; until then an external store
  would add a dependency to the auth path for a property that does not exist here.
- ✅ **Integration tests are isolated per package — fixed.** They used to share
  whatever database `PC_TEST_DATABASE_URL` named, which worked exactly once per
  database: a second run met the first run's rows and the failures looked like
  regressions rather than leftovers (chunk GC counts chunks globally; media
  variants are keyed by content hash so a thumbnail outlived the blob store that
  rendered it; the extract pipeline dedupes on a unique partial index so an
  identical upload enqueued nothing the second time). The documented workaround was
  recreating the container and passing `-p 1`: a real cost paid every run to avoid
  a fixed cost paid once. [internal/testdb](../server/internal/testdb/testdb.go)
  now creates a database per test binary and drops it afterwards, and CI runs the
  suite twice in a row against one server as the regression test.

---

## The seam: who owns what

Two developers, **Vivian** (in front of the API) and **Guru** (behind it), split
**by layer rather than by feature**, because the hard part of "two people,
independent" is not dividing features — it is making sure they almost never edit
the same file.

| Directory | Owner | Notes |
|---|---|---|
| `server/internal/**`, `server/cmd/{api,pcworker,cloudctl}` | **Guru** | handlers, files, jobs, blob, CAS, auth, DB, worker |
| `deploy/embed-sidecar/`, ML on the 4090s | **Guru** | Python inference, models |
| `deploy/**`, `scripts/**`, `docs/runbook-*` | **Guru** | compose, monitoring, ops |
| `web/**` | **Vivian** | React web app (the PWA lives here — `mobile/**` was never created) |
| `client/**` (`pcsync`) | **Vivian** | headless sync engine, separate Go module (`desktop/**` was never created; the platform-free tray half is `client/internal/tray`) |
| [api-contract.md](api-contract.md) + [openapi.yaml](openapi.yaml) | **shared** | the one shared artifact; new endpoints are **additive** and land here first |
| this document | shared | plan of record |

Coordination reduces to *"is the endpoint in the contract yet?"* — an answer in a
file, not in a meeting. Server changes are additive only; migrations are additive;
server, worker, sidecar and web bundle deploy independently.

### What actually happened — the retrospective worth keeping

- **Contract-first held completely.** Every phase's endpoint shapes landed in the
  contract before the handlers, and the front track coded against them exactly.
  Phase 7's three UIs were written months before the server half and lit up
  unchanged.
- **The mock server was never built**, and that was better. The front track used
  *graceful degradation* instead: each panel renders "not available on this server
  yet" when the endpoint 404s. A fallback is code that ships and keeps earning its
  place; a mock is scaffolding that rots. The cost worth naming: a UI that degrades
  quietly is a UI nobody notices is unfinished.
- **Contract tests arrived late, after the drift they existed to prevent.** Phase 5
  shipped a finished gallery against endpoints answering `404`, and nothing in the
  repository disagreed with anything else, because the contract documents proposed
  endpoints beside shipped ones.
- **The asymmetry the split did not anticipate.** By Phase 8 the behind-the-API
  track was a full phase ahead and thirteen route shapes were served with no
  consumer. None of it was broken, just unreachable — but "both tracks move
  independently" turned out to mean one of them can finish a phase alone and the
  phase still is not done. Twelve of the thirteen now have a client, each deleted
  from `awaitingClient` as its UI landed.
- **The lesson.** Layer ownership kept merge cost near zero, but it made
  *sequencing* invisible, and sequencing was the actual risk. The fix was not a
  process change; it was a test that fails when a route has no consumer and nobody
  has said why.

---

## The rule this document lives by

**Anything absent on purpose gets a line here, with its reason. If it is not here
and it is not built, that is a bug, not a decision.**

Two things are enforced mechanically rather than by this file: routes with no
client (`awaitingClient` in `contract_test.go`, which fails in both directions) and
every claim CI can execute. Everything else is a judgement, and a judgement can
only be written down.
