# Phase 6 — Native clients (Vivian's track) design

**Status: in progress.** This is the front-of-API track from
[roadmap-split.md](roadmap-split.md): the desktop and mobile clients that a person
actually touches. It consumes the **existing** `/api/v1` surface — no new server
endpoints — so it proceeds independently of Guru's platform work.

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

## 4. Later slices (not yet)

- Slice 3: the desktop tray shell (platform icon + menu) over this control API —
  the one piece that needs a machine with a display to finish.
- Slice 4: installers / auto-update.
- Slice 5: a mobile / PWA client speaking the same `/api/v1` surface.
