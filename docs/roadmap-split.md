# Remaining roadmap — split for two developers

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
  (`client/`), and the desktop / mobile clients that don't exist yet.

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
| `desktop/**` *(new)* | **Vivian** | tray GUI wrapping `pcsync` |
| `mobile/**` *(new)* | **Vivian** | mobile / PWA client |
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
| Thumbnail + video-transcode jobs (new `kind`s on the queue, run on the 4090s) | Photo **gallery / timeline** view in web |
| EXIF extraction → searchable/sortable metadata | Album create/reorder UI, drag-select |
| Album data model + `/api/v1/albums` endpoints | Map view from EXIF GPS; lightbox viewer |
| Media variants stored in CAS, served via existing blob path | Gallery surfaced in desktop/mobile too |

### Phase 6 — Native clients
Consumes the *existing, stable* API — almost no new server work, so Vivian can
run nearly the whole phase solo while Guru is deep in Phase 7/8.

| Guru (behind API) | Vivian (front of API) |
|---|---|
| Small: a device-list / revoke endpoint; optional push-notify hook | **Desktop tray GUI** wrapping `pcsync` (selective sync, status, conflicts) |
| (otherwise idle for this phase — spends it on Phase 7) | **Mobile / PWA** client: browse, upload, offline pin |
| | Auto-update + installer packaging |

### Phase 7 — Multi-user, sharing & collaboration
The current system is single-owner-centric; this makes it multi-tenant-safe.

| Guru (behind API) | Vivian (front of API) |
|---|---|
| User-to-user shares & shared folders | Collaboration UI: share-with-person, permissions dialog |
| RBAC + per-tag / per-folder ACLs | "Shared with me" view; activity feed |
| **Scope semantic search & tags by ACL** (today they're owner-global) | Admin console: users, quotas, sessions |
| Audit log; admin/user endpoints; quotas | Quota / usage indicators |

### Phase 8 — Advanced intelligence (the 4090s earn their keep)
| Guru (behind API) | Vivian (front of API) |
|---|---|
| Face detection + clustering ("people") jobs | **"People"** faces browser; name-a-face UI |
| Near-duplicate / similar-image detection | "Similar files" surfacing in the browser |
| **RAG chat over your own docs** (retrieval + generation on a 4090) | **Chat UI** — ask questions across your library |
| Optional fine-tuning pipeline for the tagger/embedder | Feedback controls that feed labels back |

### Phase 9 — Scale & resilience (ops-weighted, mostly Guru)
| Guru (behind API) | Vivian (front of API) |
|---|---|
| Object-storage cold tier for blobs; tiering policy | Storage/health surfacing in the admin console |
| DR automation, restore drills as code | Backup-status panel |
| Per-user quota enforcement / billing hooks | Quota + billing screens |

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
