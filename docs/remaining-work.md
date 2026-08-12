# Remaining work & known tradeoffs — an honest register

This is the current, accurate answer to "what's left and what are the sharp
edges", kept honest as work lands. It splits three things people conflate:
**shipped** (done, verify it), **open** (real work, and what blocks it here), and
**by design** (a deliberate tradeoff, not a bug to fix).

_Last reconciled against the tree: Phase 8 UIs, offline pinning, and cross-build
tooling all shipped; the backends for Phases 5–8 are implemented and the clients
are wired to them._

---

## Shipped since the last status write-up ✅

These were listed as "still to build" in an earlier summary; they are done now:

- **Mobile offline pinning** — pin a file for offline access; its bytes live in a
  Cache Storage bucket the service worker serves when the network is gone
  (`web/src/pin.ts`, `web/src/Offline.tsx`). *Runtime SW behaviour wants on-device
  verification.*
- **RAG chat, faces, similar files (front-end)** — Ask uses `/chat` (answer +
  citations); a People/faces view; "find similar" in the photo viewer.
- **Cross-platform client builds** — `client/build-release.sh` produces static,
  CGO-free binaries + checksums for linux/macOS/Windows; verified building all
  five targets. `pcsync version` reports client-vs-server.

---

## Genuinely open — and what blocks finishing it *here*

| Item | State | What it needs |
|---|---|---|
| **Native tray icon (Win/Mac)** | The platform-free tray *brain* (`client/internal/tray`) and `pcsync watch` are done and tested. | The icon+menu shell needs a CGO system-tray lib and a display — a dev machine with a GUI, which this headless CI cannot compile or exercise. |
| **Desktop installers + auto-update** | Cross-built binaries + checksums exist (above). | A `.msi`/`.pkg`/Homebrew-Scoop manifest and an in-place updater — per-OS packaging and **code-signing keys** that live outside this repo. |
| **Object-storage cold tier** | API honestly reports `tiering.enabled:false` rather than faking it. | A storage backend + tiering policy — **backend work** (the other track), not front-of-API. |
| **DR automation** | Phase-0 restore runbooks + a restore-test script exist. | Scheduled, automated failover drills — **ops/infra** work. |
| **Push delivery** | Subscriptions are stored server-side. | A sender (web-push/APNs/FCM) — a backend service. |

None of these are stalled on a decision; they're stalled on an environment
(a GUI, signing keys) or belong to the backend/ops track.

---

## By design — deliberate tradeoffs, not defects

These appear as "disadvantages", and they are real, but each is a chosen
position. Listing them as bugs would invite "fixing" them into a worse system.

- **No password/passphrase recovery.** Losing the ZFS pool passphrase or the
  restic repo password is *unrecoverable by construction* — the encryption has no
  backdoor, which is the point. Mitigation is procedural (store both in a manager
  **and** on paper), and the app already gives passkey users **recovery codes**.
  A recoverable master key would be a recoverable-by-an-attacker master key.
- **No high availability.** One server cannot have HA; the design optimises
  durability + bounded recovery (snapshots + tested offsite restore) instead.
  Chasing HA across two home machines buys complexity, not uptime.
- **Tailscale-only, no public login.** Zero public attack surface is a feature,
  not an oversight; the public plane is deliberately limited to share links.
- **Two privileged containers** (cadvisor, smartctl_exporter) — documented,
  deliberate exceptions for per-container metrics and SMART disk health.
- **Setup complexity (ZFS/Docker/Tailscale).** Real, and inherent to a
  self-hosted, own-your-data system; it is not a "download an app" product and
  does not pretend to be.

---

## Where the front-of-API track stands

Phases 5, 7, 8 UIs are implemented **and now wired to real backends** (shapes
verified against the handlers, no drift). Phase 6's client is production-shaped —
sync control plane, selective sync, `.pcsyncignore`, conflicts, transfer stats,
`watch`, `doctor`, cross-builds — with only the GUI icon shell and signed
installers left, both gated on tooling this environment lacks.
