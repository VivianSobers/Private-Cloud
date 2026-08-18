# Remaining roadmap — split for two developers

**Status: the plan held, and the two tracks have converged.** Every phase below is
✅ complete behind the API, and the front-of-API track — which trailed by a full
phase at one point — has now shipped a consumer for all but four things: a map view,
browsing into a shared folder, the platform tray icon shell, and signed installers.
Marks are per cell in §2. The slice-level ledger is [status.md](status.md); §5 is a
retrospective on how the seam actually behaved.

**Legend:** ✅ done · 🟠 partial · ❌ not built.

**Goal of this document:** carve the work after Phase 4 into two tracks that
**Vivian** and **Guru** can build *in parallel without blocking or colliding with
each other*. Phases 0–4 are complete; everything here is net-new (Phase 5+).

The hard part of "two people, independent" is not dividing features — it is
making sure the two people almost never edit the same file. So the split is **by
layer, not by feature**, with one deliberate seam between them.

---

## 0. The seam: the HTTP API contract

Everything in this codebase falls on one side or the other of the JSON API the
server already exposes under `/api/v1`:

- **Behind the API** — the Go server (`server/`), the worker, the embedding
  sidecar and ML on the 4090s, storage, migrations, deploy and ops.
- **In front of the API** — the web app (`web/`), the headless sync client
  (`client/`), and the desktop / mobile clients that did not exist yet. They still
  do not, as separate trees: mobile arrived as an installable PWA inside `web/`, and
  the desktop client is `pcsync` plus a tray shell that is the last thing missing.

These two halves touch **disjoint directories** and even different languages.
The *only* thing they share is the contract. So:

> **The one shared artifact is `docs/api-contract.md` (+ its generated OpenAPI).**
> New server endpoints are **additive** and land in the contract first. The
> front-of-API track codes against the contract — with a stub/mock server when a
> real endpoint isn't built yet — so neither track ever waits on the other's
> merge.

Coordination reduces to: *"is the endpoint in the contract yet?"* Answer is in a
file, not in a meeting.

---

## 1. Ownership by directory (the collision map)

| Directory | Owner | Notes |
|---|---|---|
| `server/internal/**` | **Guru** | API handlers, files, jobs, blob, CAS, auth, DB |
| `server/cmd/{api,pcworker,cloudctl}` | **Guru** | binaries + worker |
| `deploy/embed-sidecar/`, ML on 4090s | **Guru** | Python inference, models, fine-tuning |
| `deploy/**`, `scripts/**`, `docs/runbook-*` | **Guru** | compose, monitoring, ops |
| `web/**` | **Vivian** | React web app |
| `client/**` (`pcsync`) | **Vivian** | headless sync engine (Go, separate module) |
| `desktop/**` *(new)* | **Vivian** | ❌ never created — the tray GUI wrapping `pcsync` does not exist; its platform-free half lives in `client/internal/tray` ✅ |
| `mobile/**` *(new)* | **Vivian** | ❌ never created — the PWA was reached inside `web/` instead (manifest + service worker), which is why there is no native toolchain here |
| `docs/api-contract.md` | **shared** | additive edits; the seam — see §0 |
| `docs/roadmap-split.md` (this) | shared | plan of record |

No file appears under two owners except the contract. That is the property that
makes the tracks independent.

---

## 2. Remaining phases, each split across the seam

Rather than give one whole phase to each person (which would serialize them —
the UI for a feature can't ship before its API), **every phase is split down the
seam**. Guru builds the endpoint; Vivian builds what consumes it; they meet only
at the contract.

### Phase 5 — Photos & media
The single biggest missing consumer-cloud feature.

| Guru (behind API) | Vivian (front of API) |
|---|---|
| 🟠 Thumbnail jobs ✅; **video transcode ❌** — a video is recorded as a video and nothing more | ✅ Photo **gallery / timeline** view in web |
| ✅ EXIF extraction → searchable/sortable metadata | 🟠 Album create ✅, reorder ✅ (buttons, not drag), drag-select ❌ |
| ✅ Album data model + `/api/v1/albums` endpoints | 🟠 lightbox viewer ✅; **map view from EXIF GPS ❌** though `gps` is served |
| ✅ Media variants stored in CAS, served via existing blob path | ✅ Gallery on mobile — through the installable PWA rather than a native app, which is how `mobile/**` came never to exist |

### Phase 6 — Native clients
Consumes the *existing, stable* API — almost no new server work, so Vivian can
run nearly the whole phase solo while Guru is deep in Phase 7/8.

| Guru (behind API) | Vivian (front of API) |
|---|---|
| ✅ Device list / rename / revoke ✅ consumed by Settings; the push-subscription hook is ✅ served and ❌ uncalled — it needs a VAPID key and a sender, both behind the API | 🟠 **Desktop tray**: control socket, selective sync, conflicts, `pcsync watch`, `doctor`, cross-builds and the platform-free presenter ✅; **the icon + menu adapter ❌**, blocked on a CGO tray library and a display |
| ✅ (otherwise idle for this phase — spent on Phase 7) | ✅ **Mobile / PWA**: installable, offline app shell, and **offline file pinning** — pinned bytes in a dedicated Cache Storage bucket the service worker serves when the network is gone |
| — | 🟠 Installer packaging: unsigned Linux `.deb`/`.rpm` ✅; ❌ signed repos, `.msi`/`.pkg`, Homebrew/Scoop and an in-place updater need signing keys |

### Phase 7 — Multi-user, sharing & collaboration
The current system is single-owner-centric; this makes it multi-tenant-safe.

| Guru (behind API) | Vivian (front of API) |
|---|---|
| ✅ User-to-user shares & shared folders | ✅ Collaboration UI: share-with-person, permissions dialog |
| ✅ RBAC + per-folder ACLs, inherited from the materialised path | 🟠 "Shared with me" view ✅; **browsing into a granted folder ❌** (nothing sends `?include_shared=true`); activity feed ❌ — the audit log is admin-only |
| ✅ Semantic search & tags scoped by ACL — node rows filtered, never the vectors | ✅ Admin console: users, quotas, sessions, storage and the audit log |
| ✅ Audit log; admin/user endpoints; quotas | ✅ Quota / usage indicators |

### Phase 8 — Advanced intelligence (the 4090s earn their keep)
| Guru (behind API) | Vivian (front of API) |
|---|---|
| ✅ Face detection + clustering ("people") jobs — ❌ needs a detector sidecar this repo does not ship | ✅ **"People"** faces browser and name-a-face; ✅ face overlay + reassign in the lightbox |
| 🟠 Similar **documents** ✅ through the text-embedding space; **similar images ❌** (needs a fourth model) | ✅ "Find similar" from the photo viewer |
| ✅ **RAG chat**: retrieval and `POST /chat` with mandatory citations; ❌ the generation sidecar itself is a client and a config var | ✅ **Ask** calls `/chat` — an answer when a generator is configured, citations always |
| ❌ Optional fine-tuning pipeline for the tagger/embedder — out of scope since Phase 4 | ❌ Feedback controls that feed labels back |

### Phase 9 — Scale & resilience (ops-weighted, mostly Guru)
| Guru (behind API) | Vivian (front of API) |
|---|---|
| ❌ Object-storage cold tier for blobs; tiering policy — **not started**, deliberately | ✅ Storage tab in the admin console: accounted bytes, pool state + scrub, honest `tiering: false` |
| ❌ DR automation, restore drills as code — runbooks and `restore-test.sh` are manual | ✅ Backup freshness is on the same Storage tab |
| 🟠 Per-user quota enforcement ✅, proven end to end; **billing hooks ❌** | ❌ Quota screens exist for usage; billing screens have nothing to bill |

---

## 3. How they stay unblocked in practice

1. **Contract-first.** Before building a Phase-N feature, Guru adds its endpoint
   shapes to `docs/api-contract.md`. That unblocks Vivian immediately.
2. **Mock server for the front track.** `web/` and the clients develop against a
   thin mock that serves the contract's shapes, so UI work never waits for the
   real handler to merge.
3. **Additive server changes only.** New endpoints and new job `kind`s — never a
   breaking change to an endpoint a client already uses. Migrations are additive
   (the project already treats them that way).
4. **Contract tests are the shared safety net.** One integration suite asserts
   the real server matches the contract the clients coded against. It's the only
   place both tracks' assumptions meet, and it fails loudly when they drift.
5. **Separate deploy units.** Server/worker/sidecar deploy independently of the
   web bundle and the client binaries, so a release on one side never waits on
   the other.

---

## 4. If you'd rather split by feature instead

This document deliberately splits **by layer** because that maximizes
independence — the two people never touch the same directory. The alternative,
giving each person *whole vertical features* (both server and UI), means both
edit `server/` **and** `web/` and you get merge conflicts and cross-blocking.
Only choose vertical ownership if the two developers strongly prefer full-stack
feature ownership over staying out of each other's files; then the seam becomes
a per-feature branch discipline instead of a directory boundary.

**Recommendation: keep the layer split above.** Vivian owns everything a user
touches; Guru owns everything behind the API and on the 4090s; the contract is
the one page they both edit.

---

## 5. What actually happened — a retrospective on §3

Recorded because a plan that was mostly followed is more useful with its two
deviations written down than with neither.

**§3.1 contract-first: ✅ held completely.** Every phase's endpoint shapes landed
in `api-contract.md` before the handlers, and the front track coded against them
exactly. Phase 7's three UIs were written months before the server half and lit up
unchanged.

**§3.2 mock server: ❌ never built.** The front track used *graceful degradation*
instead — each panel renders a "not available on this server yet" state when the
endpoint 404s. That turned out better than a mock, because the fallback is code
that ships and keeps earning its place, while a mock is scaffolding that rots. It
also has a cost worth naming: a UI that degrades quietly is a UI nobody notices is
unfinished.

**§3.4 contract tests: 🟠 arrived late, after the drift they existed to prevent.**
Phase 5 shipped a finished gallery against endpoints that answered `404`, and
nothing in the repository disagreed with anything else, because the contract
deliberately documents proposed endpoints beside shipped ones. `openapi.yaml` is
now generated from the real route table, and two tests close the loop in both
directions: no client may call a route that does not exist, and no route may sit
unconsumed without a declared reason. See [phase-5-design.md](phase-5-design.md) §0.

**The asymmetry the split did not anticipate, and how it closed.** By Phase 8 the
behind-the-API track was a full phase ahead and thirteen route shapes were served
with no consumer. None of it was *broken* — just unreachable — but "both tracks move
independently" turned out to mean one of them can finish a phase alone and the phase
still is not done.

That gap has since closed: twelve of the thirteen shapes now have a client, and each
line was deleted from `awaitingClient` as its UI landed, because the test refuses to
let a stale declaration sit there. The one remaining shape is the push-subscription
hook, and it is waiting on *server* work — a VAPID key and a sender — not on a UI.
So the asymmetry reversed before it was closed, which is worth knowing: the two
tracks do not converge on their own, they converge when somebody reads the list.

**The lesson the split's own retrospective earns.** Layer ownership kept the merge
cost near zero — the two halves of five phases met at contract shapes with no drift
— but it made *sequencing* invisible, and sequencing was the actual risk. The fix was
not a process change; it was a test that fails when a route has no consumer and
nobody has said why.
