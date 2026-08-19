# Runbook — Disaster Recovery

**Status: ✅ written; 🟠 never executed end to end on this deployment.** The
placeholders in "What you need before you start" are the operator's to fill in, and
until `scripts/restore-test.sh` has passed against the real pool this document is a
plan rather than a rehearsed procedure. The parts of it that can be exercised
without an actual disaster are rehearsed monthly by `scripts/dr-drill.sh` — see
[Is this document still true?](#is-this-document-still-true) for exactly which
parts those are. ❌ Automating a restore *into production* is deliberately not
built and is not planned —
[status.md](status.md#what-is-not-done--the-whole-open-list).

**The server is gone.** Fire, theft, flood, dead motherboard, or a mistake you
can't undo. This document rebuilds the whole thing from a git repo and a backup
repository.

For anything less than total loss, use
[runbook-restore.md](runbook-restore.md) instead.

---

## What you need before you start

Verify you have all five. If any is missing, that is the problem to solve
first — the rest of this document doesn't work without them.

| Item | Where it should be | Lose it and… |
|---|---|---|
| This git repo | GitHub/GitLab, plus a local clone | rebuild configs by hand |
| **ZFS pool passphrase** | password manager **and printed, in a safe** | every byte on the disks is gone forever |
| **restic repo password** | password manager **and printed, in a safe** | every backup ever taken is gone forever |
| restic repo location + credentials | in this doc, below | you can't find your own backups |
| Replacement hardware | — | nothing to restore onto |

> The two bolded passwords have no recovery path. Not "difficult to recover" —
> **mathematically impossible.** Print them. Put them somewhere physical. Do it
> today, not after the fire.

**Fill this in once and keep it current:**

```
restic repository:  ____________________________________________
offsite host:       ____________________________________________
SSH key location:   ____________________________________________
tailnet name:       ____________________________________________
```

---

## Phase A — Base system (~1 hour)

```bash
# 1. Install Ubuntu 24.04 LTS on the system disk (NOT the data disks).

# 2. Basics
sudo apt update && sudo apt upgrade -y
sudo apt install -y zfsutils-linux sanoid restic git curl jq ca-certificates

# 3. Docker, from Docker's own repo (Ubuntu's docker.io lags badly)
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"     # log out and back in for this to apply

# 4. Tailscale — see docs/tailscale-setup.md
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --hostname=cloud --ssh
```

Getting Tailscale up early matters: it's how you reach the offsite backup host.

---

## Phase B — Storage (~30 min, plus resilver)

### If the original disks survived

```bash
sudo zpool import                # list importable pools
sudo zpool import -f tank        # -f: the pool was last used by another system
sudo zfs load-key -a             # prompts for the passphrase
sudo zfs mount -a
zpool status tank && zfs list
```

This is the good outcome — the data was never lost, only the machine around it.
Skip to Phase C.

### If the disks are gone

```bash
lsblk -o NAME,SIZE,MODEL,SERIAL          # identify the new disks
ls -l /dev/disk/by-id/                   # get stable paths

sudo ./scripts/zfs-setup.sh --dry-run /dev/disk/by-id/NEW1 /dev/disk/by-id/NEW2
sudo ./scripts/zfs-setup.sh             /dev/disk/by-id/NEW1 /dev/disk/by-id/NEW2
# On a single-disk dev box, rebuild the same way you built it:
#   sudo ./scripts/zfs-setup.sh --single /dev/disk/by-id/NEW1
```

You may reuse the old passphrase or choose a new one — the new pool has no
relationship to the old one, and restic re-encrypts independently.

---

## Phase C — Data (1–6 hours, network-bound)

```bash
sudo mkdir -p /etc/private-cloud
# Recreate the restic password file from your printed copy:
sudo tee /etc/private-cloud/restic-password >/dev/null   # paste, then Ctrl-D
sudo chmod 600 /etc/private-cloud/restic-password

export RESTIC_REPOSITORY='sftp:backup@backup-pi:/srv/restic/private-cloud'
export RESTIC_PASSWORD_FILE=/etc/private-cloud/restic-password

restic snapshots        # 1. confirm you can see your backups
restic check            # 2. confirm they're intact
restic restore latest --target /     # 3. restore
```

Restore speed is bounded by your home upload link at the *other* end. A few
hundred GB over a domestic connection is an overnight job — start it and go do
Phase D in parallel.

Then reconcile the restored paths with the new pool: restic restores absolute
paths that pointed into ZFS snapshot directories
(`/tank/configs/.zfs/snapshot/restic-.../`), so move the contents into place:

```bash
sudo rsync -a /tank/configs/.zfs/snapshot/*/ /tank/configs/
```

---

## Phase D — Stack (~30 min)

```bash
sudo mkdir -p /opt && cd /opt
git clone https://github.com/YOUR-USER/private-cloud.git private-cloud
cd private-cloud

# Recreate secrets — they are NOT in git, by design.
cp deploy/secrets/.env.example deploy/compose/.env
chmod 600 deploy/compose/.env
$EDITOR deploy/compose/.env      # TAILSCALE_IP (new!), passwords, restic target
```

`TAILSCALE_IP` will be different on the rebuilt machine. Get it with
`tailscale ip -4`. This is the most commonly missed step, and it presents as
"the whole stack starts but nothing is reachable."

```bash
sudo mkdir -p /tank/postgres/data
cd deploy/compose
docker compose config          # validate before starting anything
docker compose up -d
docker compose ps
```

Restore Postgres per [runbook-restore.md §4](runbook-restore.md#4-restore-postgres).

---

## Phase E — Protection (~20 min)

**Do not skip this.** A restored server without backups is one incident away
from repeating this entire document.

```bash
sudo ./scripts/sanoid-setup.sh

sudo cp deploy/systemd/*.service deploy/systemd/*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now restic-backup.timer restic-check.timer

# The rehearsal that keeps this document honest. host-setup.sh --timers copies
# the unit in but does not enable it, so enable it explicitly.
sudo systemctl enable --now privatecloud-dr-drill.timer

sudo ./scripts/restore-test.sh    # prove it, don't assume it
```

---

## Phase F — Verify

- [ ] `zpool status tank` — `ONLINE`, no errors
- [ ] `zfs list -t snapshot | head` — snapshots being taken
- [ ] `docker compose ps` — every service `Up`/`healthy`
- [ ] `https://cloud/grafana` reachable from another tailnet device, showing data
- [ ] Test alert arrives on your phone via ntfy
- [ ] `systemctl list-timers` — `restic-backup.timer` scheduled
- [ ] `sudo ./scripts/restore-test.sh` — passes
- [ ] `sudo ./scripts/dr-drill.sh` — passes on the rebuilt machine
- [ ] `sudo ss -tlnp` — everything bound to `100.x.y.z`, nothing on `0.0.0.0`

---

## Is this document still true?

`scripts/dr-drill.sh` runs monthly from `privatecloud-dr-drill.timer` and
rehearses the parts of this runbook that can be run without a disaster. It
restores into a scratch directory, never over live data, refuses to start if
that directory is anywhere near a live path, and removes it afterwards.

**What a passing drill proves:**

- the offsite restic repository is reachable from this machine, the password
  here opens it, it holds snapshots, and a sample of its data still checksums
- the newest snapshot is recent enough that rebuilding today would not bring
  back last month's data
- `tank/configs` and `tank/postgres` come back, with files in them
- the `pg_dumpall` dump inside that snapshot replays into a throwaway Postgres
  container and yields a database with tables in it
- the deploy tree, its git remote, the secret files and the compose definition
  that Phase D depends on are present
- how long the restore leg took — a measured number, not the estimate below

**What it does not prove, and nothing on this server should be read as proving:**

- that a machine has been rebuilt. Phases A, B and D are manual and stay manual
- that the pool passphrase and restic password exist on paper in your safe.
  Those are the two items with no recovery path, and no script can see them
- that point-in-time recovery works. The drill runs `pgbackrest check`, which
  proves WAL archiving works end to end and the repository is reachable. It
  restores nothing
- by default, that file *content* restores, because restoring every byte needs
  as much scratch space as the pool holds. `restic check` re-reads a sample of
  the repository instead, and `DR_INCLUDE_BLOBS=true` covers the rest wherever
  there is room for it

The outcome is exported as `privatecloud_dr_drill_*` and alerted on. The alert
that matters is `DrDrillTooOld`, because a rehearsal that quietly stops running
looks exactly like one that keeps passing.

```bash
sudo ./scripts/dr-drill.sh            # run it now
sudo ./scripts/dr-drill.sh --status   # when did it last pass?
```

---

## Timeline

| Phase | Duration | Can run in parallel? |
|---|---|---|
| A — base system | 1 h | — |
| B — storage | 30 min (+ resilver) | — |
| C — data restore | 1–6 h | yes, with D |
| D — stack | 30 min | yes, with C |
| E — protection | 20 min | after C+D |
| **Total** | **4–8 h realistic** | assumes hardware in hand |

Row C is the only one measured rather than estimated:
`privatecloud_dr_drill_restore_seconds` is how long the last rehearsal actually
spent restoring, and `DrDrillRestoreSlowerThanRunbook` fires when it outgrows
the range above. The other rows are still estimates, and they are estimates for
work no drill performs.

Add days, not hours, if you're waiting on replacement disks to ship. Consider
keeping a cold spare drive on a shelf — it converts a week of downtime into an
afternoon, for the price of one disk.

---

## Threat model

What this architecture defends against, and what it doesn't.

| Threat | Control | Residual risk |
|---|---|---|
| Disk failure | ZFS mirror, self-healing, SMART alerts | second disk failing during resilver |
| Silent corruption | ZFS checksums + monthly scrub | none meaningful |
| Accidental deletion | 15-min snapshots, 6-month ladder | > 6 months ago |
| Ransomware on a client | server-side read-only snapshots | attacker with root **on the server** can `zfs destroy` |
| Stolen disk / RMA | ZFS native encryption at rest | none, if the passphrase is strong |
| Stolen running server | — | **pool is unlocked and readable.** Accepted risk. |
| Internet-based attack | zero forwarded ports; tailnet-only | compromise of a tailnet device |
| Compromised container | isolated networks, non-root, `read_only`, no `docker.sock`* | cadvisor is privileged (documented exception) |
| Fire / flood / theft | offsite restic backups | ≤ 24 h of data |
| Lost passwords | printed copies in a safe | **total, unrecoverable loss** |
| **Operator error** | snapshots, backups, this runbook | the most likely failure by a wide margin |

\* cadvisor mounts `docker.sock` read-only and runs privileged, because
per-container metrics are impossible otherwise. It is the one deliberate
exception to the container isolation rules; see the comment in
[docker-compose.yml](../deploy/compose/docker-compose.yml).

**The honest summary:** on a single server there is no high availability. What
this design gives you is *durability* and a *bounded recovery time*. If the
machine dies, the service is down until you rebuild it. That is the correct
tradeoff at this scale — chasing HA with two home machines buys complexity, not
uptime.

---

## Related

- [runbook-restore.md](runbook-restore.md) — anything short of total loss
- [phase-0-checklist.md](phase-0-checklist.md) — initial build sequence
- [tailscale-setup.md](tailscale-setup.md) — network layer
