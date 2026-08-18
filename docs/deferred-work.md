# Deferred work

Everything this system deliberately does not do, in one place, with the reason.

This exists because "not built yet" and "decided against" look identical from
outside the code, and both look identical to "nobody remembered". Those three
need very different responses, and the answer for each was previously scattered
across nine design documents — findable only by someone who already knew it was
there, which is exactly the person who does not need to look.

The rule: anything absent on purpose gets a line here. If it is not here and it
is not built, that is a bug, not a decision.

**Marks used here and in every other document:** ✅ done · 🟠 partial · ❌ not
built. Everything under "Not built" below is ❌ by definition; the mark is kept on
each heading anyway so this file can be skimmed against
[status.md](status.md), which is the slice-by-slice ledger and links back here for
the *why*.

Two of these are enforced mechanically rather than by this file. Routes with no
client are declared in `awaitingClient` in `server/internal/httpapi/contract_test.go`,
which fails when one is neither consumed nor declared. Everything else below is
a judgement, and a judgement can only be written down.

---

## Not built, and deliberately so

### ❌ Object-storage cold tier (Phase 9)

`GET /api/v1/admin/storage` reports `tiering: {enabled: false}` rather than a
cold tier holding zero bytes, because a zero-byte tier would imply the feature
exists and is merely empty.

**Why not:** a half-built storage tier is the worst state this system can be in.
Content "moved to cold" by code that cannot reliably read it back is content that
is gone, and it is gone silently, and it is gone for the files least recently
touched — the ones nobody will notice for months. It also needs fsck taught about
a third storage location, which is a trap this codebase has already fallen into
twice: once when chunks arrived and `--repair` would have deleted every
deduplicated byte, and again when media variants arrived and it would have
deleted every thumbnail.

**Before starting it:** fsck accounts for the third location, and the read path
is proven against a cold store that is slow and occasionally unavailable, before
a single byte moves.

See `docs/phase-9-design.md` §1.

### ❌ Disaster-recovery automation (Phase 9)

Restore is documented and rehearsed by hand — `docs/runbook-restore.md`,
`docs/runbook-disaster-recovery.md`, `scripts/restore-test.sh`.

**Why not:** the `sudo` gates in the restore path are the operator's to tick.
Automating a restore means automating something whose failure mode is
overwriting good data with old data, and the rehearsal is worth more than the
automation until the rehearsal is boring.

### ❌ Billing hooks (Phase 9)

**Why not:** there is no second tenant. Quota exists and is enforced; the thing
billing would attach to is one person's disk.

### ❌ Streaming chat answers (Phase 8)

`POST /api/v1/chat` returns a complete answer or none.

**Why not:** citations are computed from the retrieved passages and are mandatory
— an answer that streamed ahead of its citations would be, for the duration of
the stream, exactly the unverifiable output this design refuses to produce.
Streaming is worth having and needs the citation contract solved first, not
after.

### ❌ Image-embedding similarity for photos (Phase 8)

`/similar` works on documents, through the text-embedding space Phase 4 built.
Photos have no text and so have no neighbours.

**Why not:** it needs a second model and a second vector space, and the value is
mostly already delivered by face clustering and the timeline. Worth doing;
nothing depends on it.

### ❌ Video metadata beyond "this is a video" (Phase 5)

`analyzeVideo` records that a file is video and nothing else. No duration, no
dimensions, no rotation, no thumbnail.

**Why not:** those live in MP4/MKV boxes that need a real demuxer, and the honest
options are a cgo dependency on ffmpeg or shelling out to it. Both belong behind
the same "is this deployment set up for it" switch OCR sits behind, not silently
inside the media package.

**Consequence to know about:** a video appears in the timeline ordered by upload
time rather than capture time, and its tile has no thumbnail.

### ❌ Face detection on by default (Phase 8)

`cloudctl jobs reindex --kind=faces` is opt-in and is not part of `--kind=all`.

**Why not:** it needs a detector sidecar most deployments will not run. Queueing
a job per photo on a server with no detector fills the dead-letter queue instead
of doing anything.

### ❌ Generation and detection sidecar reference implementations (Phase 8)

`deploy/embed-sidecar` is the only sidecar in this repository. The generation
sidecar (`POST /generate`, behind `PC_GENERATE_URL`) and the detection sidecar
(`POST /detect`, behind `PC_DETECT_URL`) exist here as a **Go client and a config
variable each** — the wire shape is fixed and tested, but nothing in `deploy/`
serves either one.

**Why not:** both are a model choice, and the model choice is a deployment's, not
this repository's. An embedder is small, interchangeable and the same for
everybody, which is why shipping one reference image was worth it. A generator is
a several-gigabyte decision about quality, latency and VRAM budget, and a face
detector is a decision about which biometric model an operator is willing to run.
Shipping a default for either would be choosing on the operator's behalf in the
two places where the choice matters most.

**Consequence to know about:** `POST /chat` degrades to citations alone
(`answer_unavailable: "generation_disabled"`) and the `faces` job does nothing,
until somebody stands up a service at those two URLs. Both are designed to be that
way — see [phase-8-server-design.md](phase-8-server-design.md) §0 — but "the
endpoint works and there is nothing behind it" is exactly the state this document
exists to make legible.

### ❌ Per-user API rate limiting (deferred since Phase 4)

There is a per-IP limiter on the auth and search paths and an `OwnerQueueCap` on
job enqueue. There is no per-user token bucket on the expensive endpoints.

**Why not:** it was the correct call at one trusted user. It is no longer clearly
correct — Phase 7 made a second account real, and Phase 8 made a single
authenticated request able to spend GPU time on a sidecar. This is the most
overdue item in the project, and it is listed here as deferred rather than
decided.

---

## ❌ Not built in front of the API

The behind-the-API track ran ahead. These are the consumers that do not exist yet;
the endpoints they would call are all live and are declared in `awaitingClient`.

| Missing client | Endpoint it would call | Phase |
|---|---|---|
| Device management: name a device, revoke a lost laptop | `GET/PATCH/DELETE /devices` | 6 |
| Web Push subscription from the PWA | `POST/DELETE /devices/{id}/push` | 6 |
| Platform tray icon + menu adapter (no `desktop/` directory) | the local control socket, which is built | 6 |
| Desktop installers and auto-update | — | 6 |
| Mobile offline pinning, share target | — | 6 |
| Browsing into a granted folder | `?include_shared=true` on children/search/tags | 7 |
| Per-user session management in the admin console | `GET/DELETE /admin/users/{id}/sessions` | 7 |
| A written-answer view | `POST /chat` | 8 |
| People browser, name-a-face, merge, reassign | `/people`, `/nodes/{id}/faces` | 8 |
| "More like this" affordance | `GET /nodes/{id}/similar` | 8 |
| Storage-health panel | `GET /admin/storage` | 9 |
| Map view over photo GPS | `gps` on the node — already served | 5 |

**Why not:** nothing more interesting than sequencing. The split by layer means
the two tracks land at different times, and a route arriving before its UI is the
recoverable direction. What is *not* acceptable is a route sitting unconsumed with
nobody able to say whether that is a plan, which is why the list is mechanical.

🟠 **Pointer-drag album reordering** is the one half-built item here rather than a
missing one: adding to an album and replacing the whole order in one `PATCH` both
work, but a person reorders with move-up/move-down buttons in a "Manage" mode. The
endpoint contract — replace the order wholesale, never N per-item updates — was
written for a drag and is already satisfied by the buttons.

## ❌ Ops follow-ups from Phase 0

Worth doing, never worth blocking a phase on, and still open:

| Item | Consequence of leaving it |
|---|---|
| Grafana dashboards exported to JSON and committed | `deploy/monitoring/grafana/dashboards/` holds only `.gitkeep`; a rebuilt server re-imports them by hand |
| Container images pinned to digests (+ Renovate) | `postgres:17.5-alpine` and twelve other tags can move under you |
| Real TLS via `tailscale cert` instead of `tls internal` | clients see a self-signed cert on the tailnet plane |
| UPS + NUT | an unclean shutdown on power loss |
| `unattended-upgrades` | security patches wait for a human |
| pgBackRest point-in-time recovery | Postgres recovers to the last nightly dump or ZFS snapshot, not to a chosen second |
| Encrypted-pool auto-unlock | **deliberate**: a reboot leaves everything unmounted until the passphrase is typed, so a remote reboot needs a console |

The three `sudo` restore gates in [phase-1-design.md](phase-1-design.md) §0 belong
to the same family and are tracked there: they can only be ticked on the real
server, by the operator, and no checkout can do it for them.

---

## Known limits worth stating

### 🟠 The chunk-existence oracle is closed at the cost of cross-user transfer dedup

`POST /api/v1/chunks/have` answers from the caller's own chunks only, so a
stranger is told to upload content the server already holds. Storage dedup is
unaffected — `PutKeyed` is a no-op for a key that exists — but the transfer is
paid twice.

**Why:** the global answer was a truthful yes/no about whether any given content
exists on this server, for anyone who could guess the bytes. That is a real
oracle, and bandwidth is the cheaper thing to spend.

### 🟠 Quota counts logical bytes, deduplicated, not blocks on disk

`Usage.TotalBytes` counts each distinct content once across live, trashed and
retained versions.

**Why:** it is the number a person can predict from what they uploaded. Actual
disk depends on compression ratios and on content shared with other accounts, and
charging someone for a chunk they share with a stranger is not explicable.

**Consequence:** the sum of every account's usage does not equal the pool's used
bytes, and should not be expected to.

### 🟠 Clustering is greedy and order-dependent (Phase 8)

A person photographed over years may end up in several clusters.

**Why:** re-partitioning globally on each arrival would either run constantly or
keep renaming clusters somebody has already named. A name is a promise. Merge
and reassign are the correction path, and corrections are now permanent —
`faces.dismissed_at`, migration 00024.

### 🟠 Rate limiting is in-process (Phase 4)

Correct for a single node. If the API is ever replicated, the limiter has to move
to shared state; until then an external store would add a dependency to the auth
path in exchange for a property that does not exist here.

### 🟠 Integration tests share one database

They do not fully isolate, and a stale database produces failures that look like
regressions — chunk GC counts on-disk chunks globally, media variants are shared
by content hash across fixtures whose blob stores are separate temp directories.

**Run them with a fresh Postgres container and `-p 1`.** Before concluding a
change broke something, recreate the container and run it again.
