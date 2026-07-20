# Phase 0 — Checklist

Work top to bottom. Later steps assume earlier ones. All commands run **on the
Ubuntu server**; the repo was authored on a separate machine.

**Exit criterion:** ZFS pool healthy · Tailscale connected · Docker stack up ·
Grafana dashboards showing data · ntfy test alert received · restore test
executed successfully.

---

## 0. Before you touch anything

- [ ] Ubuntu 24.04 LTS installed on a **system disk separate from the two 4 TB data disks**
- [ ] `sudo` works; system fully updated (`sudo apt update && sudo apt upgrade -y`)
- [ ] Repo cloned to `/opt/private-cloud` (paths in the systemd units assume this)
- [ ] `chmod +x scripts/*.sh`
- [ ] Verified the scripts have **LF line endings** — `head -1 scripts/zfs-setup.sh | file -`
      should not say "CRLF". `.gitattributes` handles this, but check once; a
      stray `\r` produces `bad interpreter` errors that read as nonsense.

## 1. Identify the disks — do this carefully

- [ ] `lsblk -o NAME,SIZE,MODEL,SERIAL` — confirm both 4 TB disks
- [ ] `ls -l /dev/disk/by-id/` — note the **stable** paths for each
- [ ] Write the two `/dev/disk/by-id/...` paths down here:

```
DISK1 = /dev/disk/by-id/________________________________
DISK2 = /dev/disk/by-id/________________________________
```

- [ ] Triple-check these are the empty data disks, not the system disk
- [ ] Confirm they hold nothing you want: `sudo blkid /dev/disk/by-id/...`

> `zfs-setup.sh` refuses to run on a disk with an existing filesystem
> signature. That guard is a backstop, not a substitute for checking.

## 2. ZFS pool

- [ ] Dry run first: `sudo ./scripts/zfs-setup.sh --dry-run "$DISK1" "$DISK2"`
- [ ] Read every command it prints. Understand each one.
- [ ] Real run: `sudo ./scripts/zfs-setup.sh "$DISK1" "$DISK2"`
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
- [ ] Export edited dashboards to JSON and commit them

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
- [ ] Install the timers:

```bash
sudo cp deploy/systemd/*.service deploy/systemd/*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now restic-backup.timer restic-check.timer
systemctl list-timers 'restic*'
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

- [ ] `zpool status tank` → `ONLINE`, zero errors
- [ ] Snapshots accumulating on schedule; `tank/staging` excluded
- [ ] Tailscale connected; stack reachable from phone and laptop; **nothing on `0.0.0.0`**
- [ ] `docker compose ps` → all healthy
- [ ] Grafana dashboards showing live data
- [ ] `node_systemd_unit_state` and the SMART/NVMe health series **exist in
      Prometheus** — the alerts that guard backups and disks are watching
      real metrics, not absent ones
- [ ] A test alert **received on your phone**
- [ ] A deliberately broken backup run **paged your phone with a failure**
- [ ] `restore-test.sh` passed
- [ ] ZFS passphrase and restic password exist **on paper, off this machine**

---

## Hardening follow-ups

Worth doing, not worth blocking Phase 1 on.

- [ ] **Pin images to digests.** Tags are mutable; `postgres:17.5-alpine` can
      change under you. `docker inspect --format='{{index .RepoDigests 0}}' postgres:17.5-alpine`
      then use `image: postgres@sha256:...`. Add Renovate to bump them.
- [ ] **Real TLS certs** via `tailscale cert`, replacing `tls internal` — see
      [tailscale-setup.md](tailscale-setup.md#4-enable-magicdns-and-https).
- [ ] **Backup-freshness metric.** `restic-backup.sh` alerts on failure but not
      on *never ran*. A node_exporter textfile collector writing a
      `backup_last_success_timestamp` closes the gap; the alert rule stub is in
      [alerts.yml](../deploy/monitoring/alerts.yml).
- [ ] **Pool health metric.** node_exporter's ZFS collector doesn't expose
      `zpool status`. A textfile collector running `zpool list -H -o health`
      lets you alert on `DEGRADED` rather than finding out during a scrub.
- [ ] **UPS + NUT** for clean shutdown on power loss.
- [ ] **Unattended security upgrades:** `sudo apt install unattended-upgrades`.
- [ ] **pgBackRest** for point-in-time recovery (Phase 1 — do it before there's
      data you'd miss).
- [ ] **Encrypted pool auto-unlock.** Currently a reboot leaves everything
      unmounted until you type the passphrase. That is a deliberate security
      property, not a bug — but decide consciously whether you want it, because
      it means a remote reboot needs a console.
