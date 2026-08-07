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

## 3. Later slices (not this one)

- Slice 2: the desktop tray shell (platform icon + menu) over this control API,
  plus selective sync (choose sub-folders).
- Slice 3: installers / auto-update.
- Slice 4: a mobile / PWA client speaking the same `/api/v1` surface.
