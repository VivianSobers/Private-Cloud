# Phase 0 — Checklist

**Status: ✅ every piece is built, committed, and — where a machine can check it
— checked by CI.** This is a *procedure*, not a project ledger: an unticked box
here means "you have not done this on your server yet", not "the repository is
missing something".

The hardening follow-ups at the bottom were the exception, because those were
repository work rather than operator work, and they sat unticked for nine phases.
They are done now, and they are one command:

```bash
sudo ./scripts/host-setup.sh --all      # upgrades, UPS, timers
sudo ./scripts/host-setup.sh --check    # report what is configured, change nothing
```

**What CI now proves, so you do not have to take this document's word for it**
([.github/workflows/ci.yml](../.github/workflows/ci.yml)): the alert rules parse
and pass their unit tests, every dashboard query is valid PromQL, the Caddyfile
validates in both TLS modes, every third-party image is pinned to a digest, and
— the one that matters — a ZFS pool is built on loopback files, backed up with
restic, and restored, with the drill failing the build if the bytes do not come
back. That last job is the automated form of §10 below. It does not replace
running the drill against *your* pool, which is what §10 asks for, but it does
mean the restore path is never silently broken.

Marks: ✅ done · 🟠 partial · ❌ not built; the project-wide ledger is
[status.md](status.md).

Work top to bottom. Later steps assume earlier ones. All commands run **on the
Ubuntu server**.

**Exit criterion:** ZFS pool healthy · Tailscale connected · Docker stack up ·
Grafana dashboards showing data · ntfy test alert received · restore test
executed successfully.

---

## Hardware profile: dev vs production

This checklist supports two profiles. The architecture is identical; only the
disk topology and a few memory numbers differ.

| | **Production** (target) | **Dev / experimentation** (this box) |
|---|---|---|
| Data disks | 2 × 4 TB → ZFS **mirror** | 1 disk → **single-disk** pool (`--single`) |
| Redundancy | survives one disk failure | **none** — pool is disposable; restic is the safety net |
| RAM | ~16 GB | ~7 GB |
| Postgres tuning | production profile in `.env` | low-memory defaults (no `.env` overrides) |
| ARC | auto ~4 GiB | auto ~1.7 GiB |

Where a step differs, the **(dev)** and **(prod)** variants are called out. A
single-disk dev pool upgrades to a mirror later with `zpool attach` and no
rebuild, so nothing here is throwaway work.

---

## 0. Before you touch anything

- [ ] Ubuntu 24.04 LTS installed on a **system disk separate from the data disk(s)**
      — **(prod)** the two 4 TB disks; **(dev)** whichever single disk you're testing on
- [ ] `sudo` works; system fully updated (`sudo apt update && sudo apt upgrade -y`)
- [ ] Repo cloned to `/opt/private-cloud` (paths in the systemd units assume this)
- [ ] `chmod +x scripts/*.sh`
- [ ] Verified the scripts have **LF line endings** — `head -1 scripts/zfs-setup.sh | file -`
      should not say "CRLF". `.gitattributes` handles this, but check once; a
      stray `\r` produces `bad interpreter` errors that read as nonsense.

## 1. Identify the disks — do this carefully

- [ ] `lsblk -o NAME,SIZE,MODEL,SERIAL` — **(prod)** confirm both 4 TB disks;
      **(dev)** confirm the single test disk
- [ ] `ls -l /dev/disk/by-id/` — note the **stable** paths for each
- [ ] Write the `/dev/disk/by-id/...` path(s) down here:

```
DISK1 = /dev/disk/by-id/________________________________
DISK2 = /dev/disk/by-id/________________________________   # prod only
```

- [ ] Triple-check these are the empty data disks, not the system disk
- [ ] Confirm they hold nothing you want: `sudo blkid /dev/disk/by-id/...`

> **(dev, this box):** the 500 GB drive is `/dev/sda` and currently has two
> partitions (`sda1`, `sda2`). Inspect them with `sudo blkid /dev/sda*` and
> `lsblk -f /dev/sda` **before** wiping. If they hold anything you want, stop.
> Otherwise clear the signatures so the setup guard lets you proceed:
> `sudo wipefs -a /dev/sda1 /dev/sda2 /dev/sda`.

> `zfs-setup.sh` refuses to run on a disk with an existing filesystem
> signature. That guard is a backstop, not a substitute for checking.

## 2. ZFS pool

- [ ] Dry run first — read every command it prints and understand each one:
  - **(prod)** `sudo ./scripts/zfs-setup.sh --dry-run "$DISK1" "$DISK2"`
  - **(dev)** `sudo ./scripts/zfs-setup.sh --single --dry-run "$DISK1"`
- [ ] Real run:
  - **(prod)** `sudo ./scripts/zfs-setup.sh "$DISK1" "$DISK2"`
  - **(dev)** `sudo ./scripts/zfs-setup.sh --single "$DISK1"` — note the loud
    "NO redundancy" warning; that is expected and correct for a test pool
- [ ] Choose a **strong** passphrase when prompted
- [ ] **Record the passphrase in your password manager**
- [ ] **Print the passphrase and put it somewhere physical.** No recovery exists.
- [ ] Verify: `zpool status tank` → `ONLINE`
- [ ] Verify datasets: `zfs list -o name,recordsize,compression,encryption -r tank`
- [ ] Test the lock/unlock cycle **now**, while nothing depends on it:
      `sudo zfs unload-key -a && sudo zfs load-key -a && sudo zfs mount -a`
- [ ] `sudo mkdir -p /tank/postgres/data` (Postgres needs a non-empty-root subdir)
- [ ] `sudo chown 999:999 /tank/postgres/data` — the container runs as uid 999
      and, being non-root, cannot chown a root-owned directory; without this,
      initdb fails with "permission denied" on first start

## 3. Snapshots

- [ ] `sudo ./scripts/sanoid-setup.sh`
- [ ] `systemctl list-timers sanoid.timer` — scheduled
- [ ] `zfs list -t snapshot` — initial snapshots exist
- [ ] Confirm `tank/staging` has **no** snapshots (it must be excluded)
- [ ] Come back in an hour: the ladder should be filling in

## 4. Tailscale

Full detail in [tailscale-setup.md](tailscale-setup.md).

- [ ] Installed and `sudo tailscale up --hostname=cloud --ssh` completed
- [ ] **Key expiry disabled** in the admin console for this machine
- [ ] MagicDNS + HTTPS certificates enabled for the tailnet
- [ ] `tailscale ip -4` → record it
- [ ] Tailscale installed on laptop and phone
- [ ] From another device: `ping cloud` resolves and replies
- [ ] `ufw` configured (see the tailscale doc), SSH escape hatch retained

## 5. Secrets

- [ ] `cp deploy/secrets/.env.example deploy/compose/.env`
- [ ] `chmod 600 deploy/compose/.env`
- [ ] Fill in `TAILSCALE_IP`, `TS_HOSTNAME`, `TS_TAILNET`
- [ ] Generate real passwords: `openssl rand -base64 32` (once per secret, no reuse)
- [ ] **Postgres memory profile:** **(dev)** leave the `PG_*` vars unset — the
      compose defaults are already the low-memory profile for this box.
      **(prod)** uncomment the PRODUCTION `PG_*` block in `.env`. Details in
      [.env.example](../deploy/secrets/.env.example).
- [ ] Create the alertmanager→ntfy token file as an empty placeholder — compose
      bind-mounts it, and a missing file would become a directory. The real
      token can only be minted once ntfy is running (step 8):

```bash
touch deploy/secrets/ntfy-alertmanager.token
chmod 600 deploy/secrets/ntfy-alertmanager.token
```

- [ ] `git status` — **`.env` must not appear, nor `ntfy-alertmanager.token`.**
      If either does, stop and fix `.gitignore`.

## 6. Docker stack

- [ ] Create the node_exporter textfile-collector directory **before** starting
      the stack (custom backup/pool metrics land here — see
      [custom-metrics.md](custom-metrics.md)):

```bash
sudo mkdir -p /var/lib/node_exporter/textfile
sudo chmod 0755 /var/lib/node_exporter/textfile
```

- [ ] `cd deploy/compose && docker compose config` — validates, no unset-variable errors
- [ ] `docker compose up -d`
- [ ] `docker compose ps` — all services `Up`; postgres `healthy`
- [ ] `docker compose logs postgres | tail -20` — "database system is ready"
- [ ] **Verify the binding**, the single most important security check here:
      `sudo ss -tlnp | grep -E ':(80|443)'` → must show `100.x.y.z:443`, never `0.0.0.0:443`
- [ ] From another tailnet device: `curl -k https://cloud/` returns the landing page

## 7. Monitoring

- [ ] `https://cloud/grafana` loads; log in with the `.env` credentials
- [ ] `https://cloud/prometheus/targets` — every target `UP`
      (a red `smartctl` target usually means the container lacks device access)
- [ ] Import dashboards — see [the dashboards README](../deploy/monitoring/grafana/dashboards/.gitkeep):
  - [ ] 1860 Node Exporter Full
  - [ ] 193 Docker / cAdvisor
  - [ ] 9628 PostgreSQL
  - [ ] 20204 SMART disk health
- [ ] Confirm panels show **real data**, not "No data" — that's the actual test
- [ ] **Verify the systemd metrics exist** — in `https://cloud/prometheus`,
      query `node_systemd_unit_state{name="restic-backup.timer"}`. It must
      return series (once the timer is installed in step 9; any unit name works
      as a smoke test now). If it returns nothing, the systemd collector can't
      reach D-Bus and the `BackupTimerNotRunning` alert is silently dead.
- [ ] **Verify the disk-health metrics exist** — query
      `smartctl_device_attribute{attribute_name="Reallocated_Sector_Ct"}` (SATA)
      or `smartctl_device_media_errors` (NVMe). Whichever matches your drives
      must return series, or the disk-failure alerts are watching nothing.
- [ ] **Verify the ZFS pool-health metric exists** — after the collector timer
      is installed (step 9), query `privatecloud_zpool_health{state="ONLINE"}`.
      It must return `1` for your pool. If absent, the textfile collector isn't
      wired — see [custom-metrics.md](custom-metrics.md).
- [x] ✅ Dashboards are committed and provision themselves — there is nothing to
      import by hand. `deploy/monitoring/grafana/dashboards/` holds five:
      Node Exporter Full, PostgreSQL and SMART (normalised from the community
      dashboards so they need no `${DS_PROMETHEUS}` answer at import time), a
      purpose-built Docker/cAdvisor one (dashboard 193 is schemaVersion 12, a
      2016-era layout), and **Private Cloud — Overview**, which is the one that
      could not be downloaded: pool health, backup freshness, the job queue and
      the API, all reading the same series `alerts.yml` fires on. CI parses every
      query

## 8. ntfy

- [ ] Create users and grant access (the stack ships `deny-all` by default):

```bash
docker exec -it privatecloud-ntfy ntfy user add --role=admin admin
docker exec -it privatecloud-ntfy ntfy user add alertmanager
docker exec -it privatecloud-ntfy ntfy access alertmanager 'private-cloud*' write
docker exec -it privatecloud-ntfy ntfy token add alertmanager
docker exec -it privatecloud-ntfy ntfy user add backup
docker exec -it privatecloud-ntfy ntfy access backup 'private-cloud*' write
docker exec -it privatecloud-ntfy ntfy token add backup
```

- [ ] Put the **alertmanager** token into the placeholder file from step 5,
      then restart alertmanager so it picks it up:

```bash
$EDITOR deploy/secrets/ntfy-alertmanager.token   # paste tk_... , nothing else
docker compose restart alertmanager
```

- [ ] Record the **backup** token in `backup.env` (`NTFY_TOKEN`)
- [ ] Install the ntfy app on your phone; subscribe to `private-cloud` and `private-cloud-critical`
- [ ] **Send a test alert and confirm it arrives on your phone:**

```bash
curl -u admin:PASSWORD -d "phase 0 test alert" https://cloud/ntfy/private-cloud-critical
```

- [ ] Test the full chain end to end by firing a real alert:

```bash
docker compose stop postgres     # PostgresDown fires after 2 min
# wait ~3 minutes — the notification should reach your phone
docker compose start postgres    # resolved notification follows
```

> That last test is the one people skip, and it's the one that matters. A
> monitoring stack that can't reach your phone is a dashboard, not an alert
> system.

## 9. Backups

- [ ] **Decide the restic target.** Options are documented in
      [.env.example](../deploy/secrets/.env.example). Until it lives on a
      different machine, you have a copy, not a backup.
- [ ] `sudo mkdir -p /etc/private-cloud`
- [ ] `openssl rand -base64 32 | sudo tee /etc/private-cloud/restic-password`
- [ ] `sudo chmod 600 /etc/private-cloud/restic-password`
- [ ] **Print the restic password. Put it with the ZFS passphrase.**
- [ ] Create `/etc/private-cloud/backup.env` from the template; `chmod 600`; `chown root:root`
- [ ] If using SFTP: `ssh-keygen`, `ssh-copy-id`, and verify `sudo ssh backup-pi` works **as root**
      (the timer runs as root; a key that only works for your user fails at 03:20 silently)
- [ ] `sudo BACKUP_ENV=/etc/private-cloud/backup.env ./scripts/restic-backup.sh init`
- [ ] Run one manually: `sudo ./scripts/restic-backup.sh backup`
- [ ] Confirm the success notification reached your phone
- [ ] **Break a backup on purpose and confirm the FAILURE notification arrives:**

```bash
# Point the script at a repo that doesn't exist; it must fail AND page you.
# (A copy of the env file is edited because the script sources backup.env,
# which would override a plain RESTIC_REPOSITORY= on the command line.)
sudo cp /etc/private-cloud/backup.env /tmp/backup-broken.env
sudo sed -i 's|^RESTIC_REPOSITORY=.*|RESTIC_REPOSITORY=/nonexistent|' /tmp/backup-broken.env
sudo BACKUP_ENV=/tmp/backup-broken.env ./scripts/restic-backup.sh backup; echo "exit: $?"
sudo rm /tmp/backup-broken.env
```

> A backup system whose failure path has never fired is indistinguishable
> from one that fails silently. The success notification tests half the
> plumbing; this tests the half you actually bought ntfy for.
- [ ] Install the timers (this also installs the zpool-metrics collector unit):

```bash
sudo cp deploy/systemd/*.service deploy/systemd/*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now restic-backup.timer restic-check.timer privatecloud-zpool-metrics.timer
systemctl list-timers 'restic*' 'privatecloud-*'
```

- [ ] Confirm the pool-health collector ran and wrote metrics:

```bash
sudo systemctl start privatecloud-zpool-metrics.service   # run once now
cat /var/lib/node_exporter/textfile/privatecloud_zpool.prom | grep -E 'health.*ONLINE|scrub_age'
```

## 10. Prove the restore works

**This is the exit gate. Do not skip it.**

- [ ] `sudo ./scripts/restore-test.sh` → `ALL RESTORE TESTS PASSED`
- [ ] Read the output rather than just the exit code
- [ ] Manually recover one file from a snapshot, following
      [runbook-restore.md §1](runbook-restore.md#1-browse-a-snapshot-safest) —
      practise the procedure while it's an exercise, not an emergency
- [ ] Calendar reminder: run `restore-test.sh` **quarterly**

## 11. Commit

- [ ] `git status` — no secrets staged
- [ ] Commit and push to a remote you don't host yourself
- [ ] Fill in the placeholders at the top of
      [runbook-disaster-recovery.md](runbook-disaster-recovery.md)

---

## Exit criteria

Phase 0 is done when every one of these is true:

- [ ] `zpool status tank` → `ONLINE`, zero errors (**dev:** a single `disk`
      vdev, not a `mirror` — expected; **prod:** a `mirror-0` with two members)
- [ ] Snapshots accumulating on schedule; `tank/staging` excluded
- [ ] Tailscale connected; stack reachable from phone and laptop; **nothing on `0.0.0.0`**
- [ ] `docker compose ps` → all healthy
- [ ] Grafana dashboards showing live data
- [ ] `node_systemd_unit_state` and the SMART/NVMe health series **exist in
      Prometheus** — the alerts that guard backups and disks are watching
      real metrics, not absent ones
- [ ] `privatecloud_zpool_health` and `privatecloud_backup_age_seconds` **exist
      in Prometheus** — the pool-health and backup-freshness alerts have live
      metrics to fire on (see [custom-metrics.md](custom-metrics.md))
- [ ] A test alert **received on your phone**
- [ ] A deliberately broken backup run **paged your phone with a failure**
- [ ] Each simulated failure in [custom-metrics.md](custom-metrics.md#validation--simulate-each-failure-and-watch-the-full-chain)
      (stale backup, degraded pool, failed scrub) **fired and then resolved**
- [ ] `restore-test.sh` passed
- [ ] ZFS passphrase and restic password exist **on paper, off this machine**

---

## Hardening follow-ups

Worth doing, not worth blocking Phase 1 on — which is exactly why, nine phases
later, only the two metric items had been done. A list of things that never block
anything is a list nothing ever removes an item from.

**They are all done now, and each one is a file rather than a bullet.** The two
that are still a decision rather than a task are marked as such.

- [x] ✅ **Backup-freshness metric.** Done — `restic-backup.sh` now exports
      `privatecloud_backup_last_success_timestamp`; `BackupTooOld` /
      `BackupMetricsMissing` / `BackupLastRunFailed` alert on it. See
      [custom-metrics.md](custom-metrics.md).
- [x] ✅ **Pool health metric.** Done — `scripts/zpool-metrics.sh` +
      `privatecloud-zpool-metrics.timer` export `privatecloud_zpool_health` and
      scrub freshness; `ZpoolDegraded` / `ZpoolUnavailable` / `ZpoolScrubTooOld`
      / `ZpoolScrubFailed` alert on them.
- [x] ✅ **Pin images to digests.** Done — all ten third-party images in
      `docker-compose.yml` carry `@sha256:...` beside their tag, and CI fails if
      one does not. [renovate.json](../renovate.json) moves them, because a
      pinned digest with nothing to update it is a security problem wearing a
      reproducibility costume. Previously: tags are mutable; `postgres:17.5-alpine` can
      change under you. `docker inspect --format='{{index .RepoDigests 0}}' postgres:17.5-alpine`
      then use `image: postgres@sha256:...`. Add Renovate to bump them.
- [x] ✅ **Real TLS certs** via `tailscale cert`. Done —
      [scripts/tailscale-cert.sh](../scripts/tailscale-cert.sh) issues and renews
      one, `privatecloud-tailscale-cert.timer` keeps it alive, and the Caddyfile
      imports `tls.conf` so the swap is a file and a reload rather than an edit.
      Both states are validated in CI. See
      [tailscale-setup.md](tailscale-setup.md#4-enable-magicdns-and-https).
- [x] ✅ **UPS + NUT** for clean shutdown on power loss. Done —
      [deploy/host/nut/](../deploy/host/nut/) plus `host-setup.sh --ups`. Power
      events reach the same ntfy topics as every other alert. The point is not
      uptime: ZFS survives one power cut by design, and what it survives badly is
      the second and third during the resilver after the first.
- [x] ✅ **Unattended security upgrades.** Done —
      [deploy/host/apt/](../deploy/host/apt/) plus `host-setup.sh --upgrades`,
      which also runs `unattended-upgrade --dry-run` because "I installed it" and
      "it is applying updates" are different claims. Security origins only;
      Docker, Tailscale and the ZFS/kernel pair are blacklisted, since a kernel
      that no longer matches `zfs-dkms` is a machine that boots without its pool.
- [x] ✅ **pgBackRest** for point-in-time recovery. Done —
      [Dockerfile.postgres](../deploy/compose/Dockerfile.postgres) adds the binary
      (archive_command runs *inside* the Postgres container, so a sidecar cannot
      supply it), [pgbackrest.conf](../deploy/compose/pgbackrest.conf) configures
      the repository on the pool, and
      [scripts/pgbackrest.sh](../scripts/pgbackrest.sh) plus two timers run weekly
      fulls and daily differentials. RPO goes from 24 hours to one WAL segment.
      The nightly `pg_dumpall` stays: it survives this repository being the thing
      that broke. Recovery is [runbook-restore.md §4c](runbook-restore.md).
- [x] 🟠 **Encrypted pool auto-unlock — decided, and deliberately left off.**
      [privatecloud-zfs-unlock.service](../deploy/systemd/privatecloud-zfs-unlock.service)
      exists, is not enabled, and its header sets out what each possible keyfile
      location actually buys; [scripts/zfs-unlock.sh](../scripts/zfs-unlock.sh)
      refuses a key on the root filesystem, because storing the key beside the
      ciphertext it protects is not a weaker setup, it is no setup. Currently a reboot leaves everything
      unmounted until you type the passphrase. That is a deliberate security
      property, not a bug — but decide consciously whether you want it, because
      it means a remote reboot needs a console.
