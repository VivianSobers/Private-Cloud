# Private Cloud

A self-hosted, Dropbox-style personal cloud on a single Ubuntu server.

**Current status: Phases 1–4 are complete on both sides. Phases 5–9 are complete
behind the API, and the clients have caught up on all but four things** — a map
view over photo GPS, browsing into a folder somebody shared with you, the platform
tray icon shell, and signed installers. The foundation, the API, the storage
engine, the sync engine and the intelligence tier are all built and tested.

Work happens on two parallel tracks either side of the HTTP API — see
[docs/roadmap-split.md](docs/roadmap-split.md) — which is why the roadmap below
tracks the two halves of a phase separately: a phase is only *complete* when both
the endpoint and the thing that consumes it exist.

> **[docs/status.md](docs/status.md) is the slice-by-slice ledger** — every phase,
> every slice, every deliberate omission, marked. The roadmap below is the summary
> of it.

**Legend, used in every document in this repo:** ✅ done · 🟠 partial (the row says
which half) · ❌ not built (deliberately deferred ones are in
[docs/deferred-work.md](docs/deferred-work.md)).

---

## What Phase 0 provided

- **Storage** — encrypted, compressed ZFS mirror with a tuned dataset layout
- **Snapshots** — automatic, every 15 min → 6 months, read-only, ransomware-resistant
- **Network** — Tailscale-only private plane; zero forwarded router ports
- **Services** — Postgres, Caddy, Prometheus, Grafana, Alertmanager, ntfy, exporters
- **Backups** — nightly encrypted restic to an offsite target, with push alerts
- **Recovery** — tested runbooks and a script that proves the backups are real

Everything is defined as code in this repo. A rebuilt server is a `git clone`,
a `.env`, and a restore away.

---

## Quick start

> **Read [docs/phase-0-checklist.md](docs/phase-0-checklist.md) instead of this
> section if you're actually building it.** The checklist is the real procedure;
> this is only the shape of it.

```bash
sudo apt install -y zfsutils-linux sanoid restic git curl jq
curl -fsSL https://get.docker.com | sudo sh
curl -fsSL https://tailscale.com/install.sh | sh

sudo git clone <this-repo> /opt/private-cloud && cd /opt/private-cloud
chmod +x scripts/*.sh

lsblk -o NAME,SIZE,MODEL,SERIAL       # identify your two data disks
sudo ./scripts/zfs-setup.sh --dry-run /dev/disk/by-id/D1 /dev/disk/by-id/D2
sudo ./scripts/zfs-setup.sh           /dev/disk/by-id/D1 /dev/disk/by-id/D2
sudo ./scripts/sanoid-setup.sh
sudo tailscale up --hostname=cloud --ssh

cp deploy/secrets/.env.example deploy/compose/.env && chmod 600 deploy/compose/.env
$EDITOR deploy/compose/.env           # TAILSCALE_IP is mandatory
cd deploy/compose && docker compose up -d

sudo ./scripts/restore-test.sh        # the step that makes it real
```

---

## Layout

```
.
├── deploy/
│   ├── compose/docker-compose.yml    infrastructure stack
│   ├── caddy/Caddyfile               TLS + routing (tailnet-only)
│   ├── monitoring/                   prometheus, alerts, alertmanager, grafana
│   ├── sanoid/sanoid.conf            snapshot retention policy
│   ├── systemd/                      backup timers
│   └── secrets/.env.example          template; real .env is gitignored
├── server/                           Go API, worker and CLI — see server/README.md
├── client/                           pcsync, the headless sync daemon (separate Go module)
├── web/                              React web app / installable PWA
├── scripts/
│   ├── zfs-setup.sh                  pool + datasets (destructive; --dry-run first)
│   ├── sanoid-setup.sh               snapshots
│   ├── restic-backup.sh              nightly offsite backup + freshness metric
│   ├── zpool-metrics.sh              pool-health textfile collector (systemd timer)
│   └── restore-test.sh               proves backups restore
└── docs/
    ├── status.md                     ✅/🟠/❌ every phase and slice — is it built?
    ├── remaining-work.md             what's left, what blocks it, what's by design
    ├── phase-0-checklist.md          ← start here if you're building it
    ├── roadmap-split.md              who owns what, either side of the API
    ├── deferred-work.md              everything deliberately not built, with reasons
    ├── api-contract.md               the seam: shipped surface + proposed surface
    ├── openapi.yaml                  generated from the routes; what actually responds
    ├── phase-{1..9}-design.md        why each phase looks the way it does
    ├── phase-{7,8}-server-design.md  the behind-the-API half of those two
    ├── phase-4-hardening.md          the abuse review, and what it fixed
    ├── tailscale-setup.md
    ├── custom-metrics.md             backup/pool metrics + failure-sim validation
    ├── runbook-restore.md            something is lost
    ├── runbook-disaster-recovery.md  everything is lost
    └── runbook-worker.md             OCR, search and embeddings ops
```

---

## Design decisions worth knowing

**Modular monolith, not microservices.** One developer, one machine. Every
network boundary costs serialization and distributed failure modes and buys
nothing until there's a second machine. Module boundaries are enforced in code
so extraction stays cheap later.

**ZFS, not ext4 or btrfs.** End-to-end checksums with self-healing are the only
real defence against silent bit rot, and snapshots make versioning and recovery
nearly free. For a system whose entire job is "never lose my files," a
filesystem without integrity checking is disqualifying.

**Tailscale, not port forwarding.** Zero open ports means the internet-facing
attack surface in Phase 0 is literally nothing. A public plane for share links
arrives in Phase 2, as a separate and deliberately minimal surface.

**Snapshots are not backups.** They share the disks. `restic` to a machine that
is not this one is what survives fire, theft, and both disks failing.

**No high availability.** One server can't have it. This design optimises
durability and bounded recovery time instead — the correct tradeoff at this
scale. Chasing HA across two home machines buys complexity, not uptime.

**Two containers run privileged**: cadvisor (read-only `docker.sock` —
per-container metrics are impossible otherwise) and smartctl_exporter (raw
device access — SMART is the earliest warning a mirror member is dying). Both
are documented, deliberate exceptions to the isolation rules, not oversights.

---

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 0 | Storage, network, monitoring, backups, runbooks | 🟠 **code complete** — every piece is committed; the three `sudo` restore gates in [docs/phase-1-design.md](docs/phase-1-design.md) §0 are the operator's to tick, and the Grafana dashboards, digest pinning and real TLS certs in [docs/phase-0-checklist.md](docs/phase-0-checklist.md) are still open follow-ups |
| 1 | MVP: auth (passkeys), upload/download, resumable uploads, web UI, WebDAV, search | ✅ **complete** — all 7 slices |
| **2** | CAS storage engine, versioning, dedup, share links | ✅ **complete** — all 4 slices: FastCDC+BLAKE3+zstd chunking with cross-user dedup, background migration of Phase 1 blobs, real version history (list/restore/retention), and public share links (file & folder, password/expiry/download-cap, instant revocation) on a separate Caddy plane |
| **3** | Sync engine: change journal, Go client, conflict resolution | ✅ **complete** — all 4 slices: a per-owner change journal with a gap-free cursor (`GET /changes`); a block-level delta protocol (fetch manifest, negotiate missing chunks, BLAKE3-verified chunk upload, manifest commit) so a changed file moves only its changed chunks; a headless Go sync client (`client/`, `pcsync`) — a separate pure-Go module with a SQLite state DB, initial tree reconcile, incremental journal replay, fsnotify + poll + rescan loops, and an app-password-to-device-token exchange; and lineage-based conflict resolution that never overwrites or merges — a both-sides edit or a delete-vs-edit becomes a visible `name (conflict from HOST DATE).ext` copy |
| 4 | ML: OCR, semantic search, tagging; OIDC; hardening | ✅ **complete** — see [docs/phase-4-design.md](docs/phase-4-design.md). Two tiers, one queue: the always-on box owns the API, database and blob store; intelligence runs in a separate `pcworker` that drains the job queue via `SKIP LOCKED`, so GPU workers on separate boxes over the tailnet (or a CPU fallback) do the heavy inference, pulling file bytes over the API rather than a mounted store. Content never leaves your infrastructure. All slices done: job queue + worker; OCR/text extraction folded into search (a scanned receipt is findable by a word printed on it); semantic search (a Python embedding sidecar on a GPU box, content-addressed vectors, cosine KNN, off cleanly with no sidecar); cheap explainable auto-tagging; OIDC single sign-on alongside passkeys (authorization-code + PKCE, opt-in, provisions its own non-admin users); and a [hardening pass](docs/phase-4-hardening.md) that cleared a real pgx SQL-injection CVE, added body-size caps, security headers and a written abuse review |
| **5** | Photos & media: EXIF, thumbnails, albums, timeline | 🟠 **server complete (8/8); two front-of-API slices open** — see [docs/phase-5-design.md](docs/phase-5-design.md). A `media` job kind renders thumbnails and previews and reads EXIF into a content-addressed store, so a photo that arrives twice is decoded once; `?variant=thumb\|preview` serves renditions from the existing content route, keeping ranges, ETags and the share plane; `GET /media/timeline` sorts by when the shutter fired rather than by upload date; and `/albums` gives hand-ordered collections that never move a file. `cloudctl jobs reindex --kind=media` backfills a library that predates the job. The gallery, lightbox and album views in `web/` light up against it unchanged. ❌ **Still open in front of the API:** a map view from the EXIF GPS that is already served, and pointer-drag reordering (ordering today is move-up/move-down buttons). ❌ Video metadata beyond "this is a video" needs a demuxer |
| 6 | Native clients: desktop tray, mobile/PWA | 🟠 **server side complete; the clients are production-shaped, two packaging items ❌ open** — see [docs/phase-6-design.md](docs/phase-6-design.md). Behind the API: `/devices` list/rename/revoke, where a device *is* a device-kind session so revoking one is revoking the token, plus a Web Push subscription hook the server stores and never delivers. In front: the daemon's local control socket, selective sync, the platform-free tray package + `pcsync watch`, an installable PWA with an offline app shell, offline file pinning, a device-management UI in Settings, cross-platform release builds and unsigned Linux `.deb`/`.rpm` packages are done ✅. ❌ Open: the platform tray **icon shell** (needs a CGO tray library and a display), signed installers + an in-place updater (needs code-signing keys), a PWA share target, and push **delivery** — `POST /devices/{id}/push` stores a subscription no client can create, because the server publishes no VAPID key |
| 7 | Multi-user, sharing, RBAC, admin, quotas | 🟠 **server complete (4/4); one client gap** — client in [docs/phase-7-design.md](docs/phase-7-design.md), server in [docs/phase-7-server-design.md](docs/phase-7-server-design.md). Grants inherit down a folder from the materialised path, so a share covers files that do not exist yet; shared content stays out of every existing endpoint unless a client opts in with `?include_shared=true`, which keeps pre-Phase-7 clients correct; an editor's writes land in the owner's tree and on the owner's quota; semantic search filters node rows rather than the content-addressed vectors; and an admin console provisions, quotas and disables accounts — disabling, never deleting — against an audit log of authorisation-relevant events. ❌ Still open in front of the API: browsing *into* a granted folder — nothing in `web/` sends `?include_shared=true`, so a grantee reaches shared content through the "Shared" view but cannot open a shared folder inline. ❌ Per-user API rate limiting (slice 5) remains deferred |
| 8 | Advanced intelligence: faces, similar files, RAG chat | 🟠 **both sides shipped; 2/5 server slices and two sidecars open** — client in [docs/phase-8-design.md](docs/phase-8-design.md), server in [docs/phase-8-server-design.md](docs/phase-8-server-design.md). `POST /chat` retrieves then answers with **mandatory citations**, and degrades to citations alone when no generator is configured — retrieval is the trustworthy half and is useful on its own; `/nodes/{id}/similar` reuses the same scan, so there is exactly one place the ACL meets the vector store; and a `faces` job clusters photos into unnamed `/people` that a person names, merges and corrects. Three optional sidecars, every one of which degrades with a stable code rather than a 500 — though only the embedding one has a reference implementation in `deploy/`; ❌ the generation and detection sidecars are a Go client and a config var each, so on a stock install Ask returns citations without prose and the people browser is correctly empty. ✅ In front of the API: Ask now calls `/chat` for an answer with citations, a People view names face clusters, the lightbox draws detected faces and lets you reassign one, and "find similar" runs from the viewer. ❌ Streaming answers and image-embedding similarity are slices 4 and 5, unstarted |
| 9 | Scale & resilience: cold tier, DR automation, quotas | 🟠 **partial (2/5 slices)** — see [docs/phase-9-design.md](docs/phase-9-design.md). `GET /admin/storage` reports pool health, backup freshness and queue depth by reading the **same textfile collectors the alerts scrape**, so the console and Grafana cannot disagree; quota enforcement is proven end to end, including that an editor's write into a shared folder spends the *owner's* quota. **The object-storage cold tier is not built**, and the endpoint says `tiering.enabled: false` rather than reporting an empty cold tier that would imply it exists — §1 of the design records what it would take and why half-building it is the worst option for a storage system. ✅ The admin console now has a Storage tab reading that endpoint. ❌ DR automation (slice 4) and billing hooks (slice 5) are unstarted |

Search moved into Phase 1 (slice 7) rather than waiting for Phase 2 — trigram
indexes needed nothing the storage engine had to provide first.

**How to read 🟠 partial.** Phases 5–9 are split across the API seam and the two
tracks move independently, so a phase can have a finished UI and no endpoint
behind it — or, as is now the case everywhere, finished endpoints and no UI. The
authority on what actually responds is [docs/openapi.yaml](docs/openapi.yaml),
which is generated from the server's own route table and verified against it by a
contract test — not [docs/api-contract.md](docs/api-contract.md), which also
describes endpoints that are only *proposed*.

The authority on what is **consumed** is `awaitingClient` in
[server/internal/httpapi/contract_test.go](server/internal/httpapi/contract_test.go).
It listed thirteen route shapes when it was written; twelve have since shipped a
client and were deleted from it — the test forces that, failing both on an
undeclared unconsumed route and on a stale declaration. **One shape remains:** the
Web Push subscription hook, which waits on a VAPID key and a sender rather than on
a UI. [docs/status.md](docs/status.md) carries that plus the front-of-API gaps a
route table cannot see.

---

## The two things that can permanently destroy everything

1. Losing the **ZFS pool passphrase** — every byte on the disks becomes noise.
2. Losing the **restic repository password** — every backup ever taken becomes noise.

Neither has a recovery mechanism. Not "hard to recover" — impossible. Store
both in a password manager **and printed on paper somewhere physical**, and do
it before you put anything you care about on this system.
