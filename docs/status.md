# Status — what is built, what is not

One page, one legend, every phase and every slice. If you want to know whether a
thing exists, this is the page; the phase documents say *why* it looks the way it
does, and this one says *whether it is there*.

## Legend

| Mark | Means |
|---|---|
| ✅ | **Done** — built, wired end to end, and covered by tests in this repo |
| 🟠 | **Partial** — one half exists, or it works with a stated limit. The row says which half |
| ❌ | **Not built** — no code. Either deliberately deferred (see [deferred-work.md](deferred-work.md)) or genuinely open |

Two marks that look similar and are not: a ✅ **behind the API** with no client is
recorded as 🟠 at the *phase* level, because a phase is only complete when the
endpoint and the thing that consumes it both exist. That rule comes from
[roadmap-split.md](roadmap-split.md), and it is why the later phases below get two
columns instead of one.

## How this page was derived

Read out of the tree at `5d00e08` (2026-08-12):

- the server's **route table** (`server/internal/httpapi/*.go`) — what actually answers
- **`awaitingClient`** in [contract_test.go](../server/internal/httpapi/contract_test.go) — routes served that no client calls, each with a reason, enforced by a test in both directions
- **`web/src`** and **`client/`** — what a person can actually reach
- **`deploy/`**, **`scripts/`**, **`deploy/systemd/`** — what an operator gets

The counts behind the marks, for anyone who wants to check them: **105 registered
routes** (excluding the one `/boom` fixture that lives in a test file), **13 route
shapes with no client**, **25 migrations**, **504 test functions across 73 test
files**, and one sidecar image in `deploy/` where three are configurable.

**This is not a test run.** There is no Go or Node toolchain on this checkout, so
no ✅ here was re-verified by executing anything; each rests on reading the code and
the tests that accompany it. The integration suite needs a fresh Postgres and
`-p 1` — see [deferred-work.md](deferred-work.md).

---

## Roll-up

| Phase | Scope | Behind the API | In front of it | Overall |
|---|---|---|---|---|
| 0 | Storage, network, monitoring, backups, runbooks | ✅ all of it is code in this repo | — | 🟠 code complete; the `sudo` gates are the operator's to tick |
| 1 | MVP: auth, files, resumable upload, web UI, WebDAV, search | ✅ 7/7 slices | ✅ | ✅ |
| 2 | CAS engine, versioning, dedup, share links | ✅ 4/4 slices | ✅ | ✅ |
| 3 | Sync engine: journal, delta protocol, Go client, conflicts | ✅ 4/4 slices | ✅ `pcsync` | ✅ |
| 4 | OCR, semantic search, tagging, OIDC, hardening | ✅ 6/6 slices | ✅ | ✅ |
| 5 | Photos & media: EXIF, thumbnails, albums, timeline | ✅ 8/8 server slices | 🟠 gallery and albums ship; no map view, and reordering is buttons rather than drag | 🟠 |
| 6 | Native clients: desktop tray, mobile/PWA | ✅ devices + push hook | 🟠 daemon control, selective sync, `watch` and the PWA shell ✅; platform tray icon, installers, offline pinning and the device UI ❌ | 🟠 |
| 7 | Multi-user, sharing, RBAC, admin, quotas | ✅ 4/4 slices | 🟠 share-with-people, shared-with-me and the admin console ✅; shared-folder browsing and session management ❌ | 🟠 |
| 8 | Advanced intelligence: faces, similar files, RAG chat | 🟠 3/5 slices — streaming and image similarity ❌ | 🟠 Ask (retrieval) ✅; chat answers, people browser and similar-files ❌ | 🟠 |
| 9 | Scale & resilience: cold tier, DR automation, quotas | 🟠 storage health and quota ✅; cold tier, DR automation and billing ❌ | ❌ no storage panel in the admin console | 🟠 |

The honest one-line summary: **Phases 1–4 are finished on both sides; Phases 5–9
are finished behind the API and trail in front of it.** That is the direction the
split was designed to fail in — a route arriving before its UI is recoverable, and
a UI calling a route that does not exist is what happened in Phase 5 and is what
the contract test now prevents.

---

## Phase 0 — Foundation

Everything is defined as code and committed. What is unticked needs `sudo` on the
real server, which no checkout can verify for you.

| Item | Status |
|---|---|
| ZFS pool + dataset layout (`scripts/zfs-setup.sh`) | ✅ |
| Snapshot ladder (`scripts/sanoid-setup.sh`, `deploy/sanoid/`) | ✅ |
| Tailscale-only plane, zero forwarded ports (`deploy/caddy/Caddyfile`) | ✅ |
| Docker stack: Postgres, Caddy, Prometheus, Grafana, Alertmanager, ntfy, exporters | ✅ |
| Nightly encrypted restic backup + freshness metric | ✅ |
| Pool-health textfile collector + systemd timer | ✅ |
| Alert rules, with rule tests (`alerts.yml`, `alerts_test.yml`) | ✅ |
| Runbooks: restore, disaster recovery, worker | ✅ |
| `scripts/restore-test.sh` **executed against the real pool** | ❌ operator gate — [phase-1-design.md](phase-1-design.md) §0 |
| Snapshot ladder confirmed filling in; one restic backup restored | ❌ operator gate |
| Grafana dashboards exported to JSON and committed | ❌ `deploy/monitoring/grafana/dashboards/` holds only `.gitkeep` |
| Images pinned to digests (+ Renovate to bump them) | ❌ tags only — `postgres:17.5-alpine` can move under you |
| Real TLS via `tailscale cert` instead of `tls internal` | ❌ |
| UPS + NUT · unattended-upgrades · pgBackRest PITR | ❌ hardening follow-ups; never blocked a phase |
| Backup-freshness metric · pool-health metric | ✅ both landed as follow-ups |

## Phase 1 — MVP

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

## Phase 2 — Storage engine

| Slice | Contents | Status |
|---|---|---|
| 1a | Chunk store: FastCDC + BLAKE3 + zstd behind `blob.Store` | ✅ |
| 1b | All three write paths route through the chunker | ✅ |
| 2 | Chunk GC, refcount recompute, CAS-aware `fsck`, blob migration, dedup stats | ✅ |
| 3 | Version history: list, restore, retention, UI | ✅ |
| 4 | Share links: public plane, tokens, password, expiry, cap, revocation, UI | ✅ |

## Phase 3 — Sync engine

| Slice | Contents | Status |
|---|---|---|
| 1 | Change journal: per-owner counter, trigger, `GET /changes`, retention | ✅ |
| 2 | Delta protocol: manifest, `chunks/have`, verified `PUT /chunks`, commit | ✅ |
| 3 | Go sync client: SQLite state, initial sync, fsnotify + rescan, push/pull | ✅ |
| 4 | Conflict resolution: lineage detection, conflict copies | ✅ |

## Phase 4 — Intelligence, identity, hardening

| Slice | Contents | Status |
|---|---|---|
| 1 | Job queue + `pcworker` | ✅ |
| 2 | OCR / text extraction into `doc_text`, folded into trigram search | ✅ |
| 3 | Semantic search: embedding sidecar, packed float32, cosine KNN | ✅ |
| 3b | Auto-tagging: MIME category + curated vocabulary, reversible | ✅ |
| 4 | OIDC login alongside passkeys, opt-in | ✅ |
| 5 | Hardening pass — [phase-4-hardening.md](phase-4-hardening.md) | ✅ |
| — | Embedding sidecar reference implementation (`deploy/embed-sidecar`) | ✅ |
| — | pgvector / HNSW index | ❌ deferred by design; the exact scan is correct at this scale |
| — | Per-user API rate limiting | ❌ deferred since slice 5 and now the most overdue item in the project |

## Phase 5 — Photos & media

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
| 10 | Add-to-album and reordering in `web/` | 🟠 add-to-album ✅ and whole-order `PATCH` ✅, but ordering is move-up/move-down buttons in a "Manage" mode — **no pointer drag** |
| 11 | Map view from EXIF GPS | ❌ `gps` is served on the node; no map component exists |
| — | Video metadata beyond "this is a video" | ❌ needs a demuxer; a video has no thumbnail and sorts by upload time |
| — | `cloudctl jobs reindex --kind=media` backfill | ✅ |
| — | `fsck`/GC account for variant bytes | ✅ the dangerous one — `--repair` would have deleted every thumbnail |

## Phase 6 — Native clients

**Behind the API** — one column, one table, five routes, as the split predicted:

| Item | Status |
|---|---|
| `device_name` column (`00021`); a device *is* a device-kind session | ✅ |
| `GET /devices`, `PATCH /devices/{id}`, `DELETE /devices/{id}` | ✅ served — ❌ no client calls them |
| `POST`/`DELETE /devices/{id}/push` — subscription storage | ✅ served — ❌ the PWA does not subscribe |
| Device sessions forbidden from all five device routes (the escalation fix) | ✅ |
| Push **delivery** (APNs/FCM) | ❌ deliberately not this server's job; a client that registers nothing polls `/changes` |

**In front of the API:**

| # | Slice | Status |
|---|---|---|
| 1 | Local control socket: `/v1/status`, `/conflicts`, `/sync`, `/pause`, `/resume` | ✅ |
| 2 | Selective sync: excludes in both directions, persisted, `pcsync exclude` | ✅ |
| 3 | Tray presentation: platform-free `internal/tray` + `pcsync watch` | ✅ |
| 3b | Platform tray **icon + menu adapter** | ❌ no `desktop/` directory exists |
| 4 | Conflict list + dismiss, transfer tallies, `.pcsyncignore`, `pcsync doctor` | ✅ shipped, and not in the original slice plan |
| 5 | Installable PWA: manifest, icon, offline app-shell service worker | ✅ |
| 6 | Desktop installers + auto-update | ❌ |
| 7 | Mobile polish: offline file pinning, share target, push delivery | ❌ |
| — | Device management UI (name or revoke a device from the web app) | ❌ Settings shows *sessions*, not the `/devices` surface |

## Phase 7 — Multi-user, sharing, RBAC, admin, quotas

**Behind the API:**

| # | Slice | Status |
|---|---|---|
| 1 | `grants` + `audit_log` (`00022`), `AccessFor`, path-prefix inheritance | ✅ 16 tests |
| 2 | Grant endpoints, `/shared`, `?include_shared=` on children/search/tags | ✅ 11 tests |
| 3 | Admin users, sessions, audit endpoints | ✅ 8 tests |
| 4 | Editor writes: owner-charged quota, move and delete semantics | ✅ 7 tests |
| 5 | Per-user API rate limiting | ❌ still deferred |
| — | Last-admin guard | 🟠 the guard exists; its **refusal path** has no integration test, because the suite shares one database — recorded, not papered over |

**In front of the API:**

| Item | Status |
|---|---|
| Share-with-people panel ([PeopleShare.tsx](../web/src/PeopleShare.tsx)) | ✅ |
| Shared-with-me view ([SharedWithMe.tsx](../web/src/SharedWithMe.tsx)) | ✅ |
| Admin console: users, quotas, audit log ([Admin.tsx](../web/src/Admin.tsx)) | ✅ |
| Quota / usage indicators (`GET /usage`) | ✅ |
| Browsing **into** a granted folder (`?include_shared=true` in the file browser) | ❌ `include_shared` appears nowhere in `web/src` |
| Per-user session management in the admin console | ❌ `GET`/`DELETE /admin/users/{id}/sessions` served, unconsumed |

## Phase 8 — Advanced intelligence

**Behind the API:**

| # | Slice | Status |
|---|---|---|
| 1 | `/nodes/{id}/similar` + the shared retrieval layer | ✅ 8 tests — ❌ no client |
| 2 | `POST /chat`: retrieval, generation client, mandatory citations, degraded modes | ✅ 7 tests — ❌ no client |
| 3 | Face schema (`00023`–`00025`), detector client, `faces` job, clustering, `/people` | ✅ 8 tests — ❌ no client |
| 4 | Streaming answers (`stream: true`, SSE) | ❌ the citation contract comes first |
| 5 | Image-embedding similarity for photos | ❌ needs a fourth model and a second vector space |
| — | Generation sidecar reference implementation | ❌ the Go client and `PC_GENERATE_*` config exist; nothing in `deploy/` serves `POST /generate` |
| — | Detection sidecar reference implementation | ❌ same shape — Go client and `PC_DETECT_*` exist; nothing serves `POST /detect` |
| — | `cloudctl jobs reindex --kind=faces` | ✅ opt-in, deliberately outside `--kind=all` |

**In front of the API:**

| Item | Status |
|---|---|
| Ask your library ([Ask.tsx](../web/src/Ask.tsx)) — retrieval over `/search?semantic=true` | ✅ |
| Generated answers against `POST /chat` | ❌ the endpoint shipped; the view still calls `/search` |
| People / faces browser, name-a-face, merge, reassign | ❌ |
| "Similar files" affordance in the browser and gallery | ❌ |
| Feedback controls that feed labels back | ❌ |

## Phase 9 — Scale & resilience

| # | Slice | Status |
|---|---|---|
| 1 | `GET /admin/storage` from the same collectors the alerts read | ✅ 5 parser tests — ❌ no admin UI |
| 2 | Quota enforcement end to end, including owner-charged editor writes | ✅ 6 tests |
| 3 | Object-storage cold tier + tiering policy | ❌ **not started**, and honestly absent: the endpoint reports `tiering.enabled: false` rather than an empty cold tier |
| 4 | DR automation / restore drills as code | ❌ the runbooks and `restore-test.sh` exist and are manual |
| 5 | Billing hooks | ❌ not started; there is no second tenant |

---

## Served, with no client yet

The authoritative list is `awaitingClient` in
[contract_test.go](../server/internal/httpapi/contract_test.go), and a test fails
both when a route is unconsumed *and* undeclared, and when a declaration goes
stale. Reproduced here so this page and that test can be compared by eye:

| Route | Waiting on |
|---|---|
| `/devices`, `/devices/*` | the Phase 6 device management UI |
| `/devices/*/push` | the PWA subscribing to Web Push |
| `/people`, `/people/*`, `/people/*/merge` | the Phase 8 people browser |
| `/nodes/*/faces`, `/nodes/*/faces/*/reassign` | Phase 8 face overlay and correction |
| `/nodes/*/similar` | Phase 8 more-like-this |
| `/chat` | the Phase 8 answer UI (Ask still calls `/search?semantic=true`) |
| `/admin/storage` | the Phase 9 storage panel |
| `/admin/users/*/sessions`, `/admin/users/*/sessions/*` | sign-out-everywhere in the admin console |

Thirteen route shapes. Deleting a line from that map is what shipping a client
looks like.

## Deliberately absent

Every ❌ above that is a *decision* rather than a gap has its reasoning in
[deferred-work.md](deferred-work.md): the cold tier, DR automation, billing,
streaming chat, image similarity for photos, video metadata, faces-on-by-default,
and the known limits — the closed chunk oracle, logical-byte quota, greedy
clustering, in-process rate limiting, the shared test database.

The rule that file states, restated here because this is the page someone reads
first: **anything absent on purpose gets a line there. If it is not there and it
is not built, that is a bug, not a decision.**
