# Private Cloud

A self-hosted, Dropbox-style personal cloud on a single Ubuntu server.

**Current status: Phase 0 — Foundation.** Infrastructure only. No application
code exists yet, by design: every later phase depends on this layer being
trustworthy, and the fastest way to lose data is to build features on storage
you haven't proven you can restore.

---

## What Phase 0 provides

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
├── server/                           Go API (Phase 1) — see server/README.md
├── scripts/
│   ├── zfs-setup.sh                  pool + datasets (destructive; --dry-run first)
│   ├── sanoid-setup.sh               snapshots
│   ├── restic-backup.sh              nightly offsite backup + freshness metric
│   ├── zpool-metrics.sh              pool-health textfile collector (systemd timer)
│   └── restore-test.sh               proves backups restore
└── docs/
    ├── phase-0-checklist.md          ← start here
    ├── tailscale-setup.md
    ├── custom-metrics.md             backup/pool metrics + failure-sim validation
    ├── runbook-restore.md            something is lost
    └── runbook-disaster-recovery.md  everything is lost
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
| 0 | Storage, network, monitoring, backups, runbooks | **complete** |
| **1** | MVP: auth (passkeys), upload/download, web UI, WebDAV | **in progress** — slice 1 of 7 |
| 2 | CAS storage engine, versioning, dedup, share links, search | not started |
| 3 | Sync engine: change journal, Go client, conflict resolution | not started |
| 4 | ML: OCR, semantic search, tagging; OIDC; hardening | not started |

---

## The two things that can permanently destroy everything

1. Losing the **ZFS pool passphrase** — every byte on the disks becomes noise.
2. Losing the **restic repository password** — every backup ever taken becomes noise.

Neither has a recovery mechanism. Not "hard to recover" — impossible. Store
both in a password manager **and printed on paper somewhere physical**, and do
it before you put anything you care about on this system.
