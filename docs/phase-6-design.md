# Phase 6 — Native clients design

**Status: server side complete; client side has two slices open.** The bulk of
this phase is the front-of-API track from [roadmap-split.md](roadmap-split.md):
the desktop and mobile clients a person actually touches, which consume the
**existing** `/api/v1` surface. The split promised "almost no new server work",
and that held — the behind-the-API half is one column, one table and five
endpoints, described in §7.

**Exit criterion:** a person can install a desktop app, sign in once, pick a
folder, and see — at a glance, from the system tray — whether their files are up
to date, pause syncing when they're on a hotspot, force a sync, and find the
files that ended up in a conflict. Closing the window keeps syncing; the app is
the *window onto* the daemon, not the daemon itself.

---

## 0. The daemon already exists; it just can't be watched or steered

`pcsync` (the `client/` module) already reconciles a folder in both directions
over the delta protocol. What it lacks is everything a GUI needs:

- **Observation** — it logs to stderr and nothing else. A tray needs a live
  answer to "are we up to date, syncing, paused, or broken, and when did we last
  succeed?" without scraping logs.
- **Control** — there is no way to pause, resume, or force a sync from outside
  the process.
- **A conflict view** — conflict copies are created and logged, then forgotten.
  A person needs the list of "these files need your attention."

So slice 1 is **not** the tray icon. It is the *contract between a GUI and the
daemon*: a small local control surface the daemon exposes and any front-end —
a tray shell, a CLI, later a mobile companion — drives. Build the substance
first; the platform-specific icon is a thin shell over it.

## 1. The seam: a local Unix-socket control API

The daemon serves a tiny HTTP API over a **Unix domain socket** in the state
directory (`<state>/control.sock`), never a TCP port:

- A socket is gated by filesystem permissions (0600, owner-only) — no port means
  nothing on the network, not even loopback, can reach it. On a shared machine
  another user cannot pause your sync.
- It is the same shape a mobile companion or a future web-local UI can speak, so
  the GUI work is not throwaway.

Endpoints (all JSON, versioned under `/v1`):

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/status` | phase, last sync, last error, tracked-item count, uptime |
| `GET` | `/v1/conflicts` | recent conflict copies needing a human decision |
| `POST` | `/v1/sync` | force one reconcile now (honoured even while paused) |
| `POST` | `/v1/pause` | stop automatic syncing (timers + file events) |
| `POST` | `/v1/resume` | resume automatic syncing |

## 2. What the engine has to grow

- **A status snapshot.** The engine records its own phase transitions
  (idle → syncing → idle/error) and the timestamp and error of the last run,
  behind a mutex, so the control goroutine can read a consistent snapshot while
  the sync loop runs.
- **Control inputs.** `Pause`/`Resume` (an atomic flag the loop checks before
  acting on a tick or a file event) and `SyncNow` (a buffered trigger the loop's
  `select` listens on). Pause stops the *automatic* cadences only; an explicit
  `SyncNow` still runs, so "paused" never means "stuck".
- **A conflict log.** `conflictCopy` already knows the original and the copy path
  at the moment it sets one aside; it appends that to a bounded in-memory ring
  the status surface exposes. Bounded, because it is a "needs attention" hint,
  not an audit trail — the files themselves are the durable record.

None of this changes how reconciliation *decides* anything: the lineage-based
conflict rule, the pull-before-push order, the two-hash state model are all
untouched. This slice only makes the existing behaviour observable and steerable.

## 3. Slice 2 — Selective sync ✅

A device need not carry the whole tree. Selective sync is a set of excluded
server-path prefixes, honoured on both directions and enforced as a **local**
decision — an excluded subtree is never downloaded, a file created under one is
never uploaded, and its absence never deletes it on the server, so every device
holds its own subset of one authoritative tree.

Landed as: an atomic exclude set on the engine read per node on the sync path
(`selective.go`); pull, push and deletion all skip excluded paths; a
`pruneExcluded` transition handler that reclaims a newly-excluded clean subtree
locally but leaves one with unpushed edits on disk (never destroying an edit, and
never touching the server); persistence in the state db so a live change outlives
a restart, with the config's `excludes` as first-run seed only; `GET`/`PUT
/v1/excludes` on the control socket; and `pcsync exclude list|add|remove`.

## 4. Slice 3 — Tray presentation + live monitor ✅ (icon shell pending)

The substance of the tray — turning daemon status into what a person sees — is a
platform-free package (`internal/tray`): a `State` (offline/error/paused/
syncing/idle, in precedence order), a one-line `Summary` for the tooltip, glyphs,
and the shared relative-time formatter. It is fully unit-tested.

`pcsync watch` renders that live over the control socket — a self-refreshing
status line, the headless counterpart to a tray icon:

```
✓  Up to date — 1284 items · last sync 8s ago
```

**What's left for a display machine:** only the platform icon+menu adapter. It
maps `tray.State` to an icon and `tray.Summary` to a tooltip, and wires menu
items to the existing control client (`Pause`/`Resume`/`Sync`/`SetExcludes`) —
all of which are built and tested here. The GUI is genuinely a thin shell; every
decision it makes is decided in `internal/tray` and exercised without a screen.

## 5. Slice 5 — Installable PWA (foundation) ✅

The web app is now an installable Progressive Web App: a manifest and icon make
it addable to a home screen or desktop, and a narrow service worker
(`web/public/sw.js`) makes its shell open offline without ever caching
authenticated data — `/api/*` is never intercepted, navigations are network-first
with an offline shell fallback, and content-addressed `/assets/*` are cache-first.
This is the mobile client's foundation reached with no native toolchain: the same
`/api/v1` surface, installed as an app. Native-feeling polish (offline file
pinning, push) is a later slice.

## 5b. Slice 6 — Offline file pinning ✅

Mobile polish landed: a file can be **pinned for offline access**. Its bytes are
stored in a dedicated Cache Storage bucket (`pc-pinned`, [web/src/pin.ts](../web/src/pin.ts))
and the service worker serves that bucket when the network is gone —
network-first for freshness and to revalidate auth, cache on failure. A small
localStorage index remembers *which* files are pinned so the **Offline** view can
list them, since Cache Storage keys on URLs alone. Pin/unpin lives in the photo
viewer; the shell-cache version bump deliberately preserves the pin bucket so an
app update never evicts a user's offline files. (Runtime service-worker behaviour
wants on-device verification; the logic is conservative and only ever touches the
bare content URL a pin created.)

## 6. Later slices (not yet)

- The platform tray icon adapter + desktop installers / auto-update.
- Push **delivery** (the subscription is stored server-side; something still has
  to send) and a share-target.

---

## 7. The behind-the-API half ✅

Small, as the split predicted, but with one sharp edge.

**A device is a session of kind `device`** — the row `POST /auth/token` already
mints. Nothing new represents one. A separate table keyed to the session would
have to be kept in step with revocation, and the whole reason "I lost my laptop"
works is that revoking the session **is** revoking the device, with no second
place for the two to disagree.

What a session lacked was a human name: `user_agent` is whatever the client
called itself, frequently `Go-http-client/2.0`. That is one added column
(`00021`), not a table.

`platform` and `app_version` are **parsed from the stored agent at read time**
rather than stored. They are self-reported, the contract marks them advisory, and
deriving them keeps that obvious while leaving nothing to migrate when the parser
improves. An unrecognised agent yields empty rather than a guess — a wrong
platform is worse than none, because it makes the list look authoritative when it
is not. Android is checked before Linux, since Android agents contain both.

| Route | Purpose |
|---|---|
| `GET /devices` | list, with `current` marking the caller's own |
| `PATCH /devices/{id}` | rename "unknown device" to "the laptop" |
| `DELETE /devices/{id}` | revoke the token; effective on the next request |
| `POST`/`DELETE /devices/{id}/push` | register / unregister a Web Push endpoint |

### The sharp edge: this plane is credential management

`requireAuth` already confines a device session away from credentials — an app
password cannot mint another credential, so a token exchanged from one must not
either. That check was written as a prefix test on `/api/v1/auth/`, and
`/devices` is not under it.

But `DELETE /devices/{id}` revokes a token and `PATCH` renames one. As first
written, these routes would have let a **single leaked app password revoke every
other device on the account** — the exact escalation the existing rule prevents,
one path prefix to the side. The check is now about what a path *does* rather
than where it lives, and a device token gets `403` from all five routes while
keeping the file and sync planes it actually needs.

Push is confined for the same reason: a device registering an endpoint against a
*sibling* device's id would redirect that device's notifications. Nothing
legitimate is lost — the PWA that wants push holds a cookie session.

### Push is a hook, not a service

This server does not talk to APNs or FCM and should not learn to. It stores what
a client registered so something else can deliver. A client that registers
nothing polls `GET /changes`, which is the existing working path, so push is a
latency optimisation and **never** a correctness requirement.

Revocation drops the subscription explicitly, and this is worth recording because
the first implementation got it wrong and a test caught it: revoking a session is
a **soft delete** that stamps `revoked_at` and removes no row, so the foreign
key's `ON DELETE CASCADE` never fires. A revoked device stayed a delivery target —
still receiving notifications about a library it could no longer read, which is
the one thing "I lost my laptop" most needs not to happen. Both now happen in one
statement, so a failure between them cannot leave a live subscription against a
dead session.
