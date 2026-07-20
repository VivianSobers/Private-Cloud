# Runbook — Restore

**Read this when something is lost.** Procedures are ordered from least to most
destructive. Work down the list; stop as soon as you have your data back.

> **Rule zero:** if you are unsure, take a snapshot before you act.
> `sudo zfs snapshot -r tank@before-i-touch-anything`
> It costs nothing and it has saved more data than every clever recovery
> technique combined.

---

## Decision table

| What happened | Go to | Expected recovery time |
|---|---|---|
| Deleted or overwrote a file | [§1 Browse a snapshot](#1-browse-a-snapshot-safest) | < 1 minute |
| Bad change across a whole dataset | [§2 Rollback](#2-rollback-a-dataset-destructive) | < 5 minutes |
| Need old + new side by side | [§3 Clone](#3-clone-a-snapshot-non-destructive) | < 5 minutes |
| Postgres is corrupt / wrong data | [§4 Restore Postgres](#4-restore-postgres) | 5–30 minutes |
| A disk died | [§5 Replace a disk](#5-replace-a-failed-mirror-disk) | 10 min + hours resilver |
| **The whole pool is gone** | [§6 Restore from restic](#6-restore-from-restic-pool-is-gone) | 1–6 hours |
| **The server is gone** | [runbook-disaster-recovery.md](runbook-disaster-recovery.md) | 4–24 hours |

---

## 1. Browse a snapshot (safest)

Every dataset exposes its snapshots read-only under `.zfs/snapshot/`. Nothing is
modified — you're just copying out of the past.

```bash
ls /tank/blobs/.zfs/snapshot/
```

If that directory appears empty, the control directory is hidden (the default):

```bash
sudo zfs set snapdir=visible tank/blobs
```

Find and copy the file back:

```bash
# Which snapshots contain it?
ls -la /tank/configs/.zfs/snapshot/*/path/to/file.txt

# Copy the version you want back into place.
sudo cp /tank/configs/.zfs/snapshot/autosnap_2026-07-20_03:00:00_hourly/path/to/file.txt \
        /tank/configs/path/to/file.txt
```

Snapshot names encode their creation time and retention tier, so
`autosnap_2026-07-20_03:00:00_hourly` is exactly what it looks like.

**This handles the overwhelming majority of real incidents.** Try it first,
every time.

---

## 2. Rollback a dataset (destructive)

Rollback returns an entire dataset to a point in time and **permanently
discards every change made since**, including snapshots newer than the target.

```bash
zfs list -t snapshot -o name,used,refer,creation -r tank/configs   # 1. choose

sudo zfs snapshot tank/configs@pre-rollback-$(date +%Y%m%d%H%M)     # 2. safety net

sudo zfs rollback -r tank/configs@autosnap_2026-07-20_03:00:00_hourly  # 3. go
```

`-r` destroys intermediate snapshots. Without it, ZFS refuses to roll back past
a newer snapshot — a guardrail, so read the error rather than reflexively
adding `-r`.

Stop anything using the dataset first (`docker compose down` for
`tank/postgres`); rolling back underneath a running process gives it a
filesystem that changed without warning.

---

## 3. Clone a snapshot (non-destructive)

When you need the old state *and* the current state simultaneously — comparing
before committing to a rollback, or recovering selectively.

```bash
sudo zfs clone tank/configs@autosnap_2026-07-20_03:00:00_hourly tank/configs-recovered
ls /tank/configs-recovered

# ... copy out what you need, then:
sudo zfs destroy tank/configs-recovered
```

A clone is instant and initially free — it shares blocks with its origin and
only consumes space as it diverges. Note the dependency: the origin snapshot
cannot be destroyed while the clone exists (`zfs promote` breaks that link if
you decide to keep the clone permanently).

---

## 4. Restore Postgres

### 4a. From the nightly logical dump

```bash
cd deploy/compose
docker compose stop postgres

# Confirm the dump is intact before destroying anything:
zcat /tank/configs/db-dumps/pgdumpall-20260720.sql.gz | head -5

docker compose up -d postgres
sleep 10
zcat /tank/configs/db-dumps/pgdumpall-20260720.sql.gz \
  | docker exec -i privatecloud-postgres psql -U privatecloud
```

`pg_dumpall` output includes `CREATE DATABASE` and role definitions, so it
restores a whole cluster into an empty one.

### 4b. From a ZFS snapshot of the data directory

Faster, and preserves everything the dump loses (exact WAL position, extension
state). Crash-consistent: Postgres will replay WAL on start, exactly as after a
power cut.

```bash
cd deploy/compose && docker compose down
sudo zfs snapshot tank/postgres@pre-restore-$(date +%Y%m%d%H%M)
sudo zfs rollback -r tank/postgres@autosnap_2026-07-20_03:00:00_hourly
docker compose up -d postgres
docker compose logs -f postgres      # expect "database system is ready"
```

If it doesn't come up, fall back to 4a.

> **Gap worth naming:** neither path gives point-in-time recovery. Restoring to
> "10 minutes before the bad `DELETE`" needs continuous WAL archiving
> (pgBackRest). Phase 0's RPO for Postgres is **24 hours** from the dump, or
> **15 minutes** from ZFS snapshots. Add pgBackRest in Phase 1, before there is
> data you'd miss.

---

## 5. Replace a failed mirror disk

You'll learn about this from the `SmartDeviceUnhealthy` alert, ideally before
the disk actually dies.

```bash
zpool status -v tank        # 1. identify the bad device
```

If the pool still reads `ONLINE`/`DEGRADED`, **you have not lost data** — the
mirror is doing its job. Don't panic, and don't rush the next step.

```bash
sudo zpool offline tank /dev/disk/by-id/OLD-DISK-ID     # 2. take it offline
# 3. physically swap the drive
sudo zpool replace tank /dev/disk/by-id/OLD-DISK-ID \
                        /dev/disk/by-id/NEW-DISK-ID     # 4. resilver
watch -n 30 zpool status tank                            # 5. wait
```

Resilvering 4 TB takes several hours to a day. **The pool has no redundancy
until it finishes** — a second failure during resilver loses everything. This
is precisely when you verify your restic backup is current, not after.

---

## 6. Restore from restic (pool is gone)

Both disks failed, the pool is unimportable, or the machine burned down.

```bash
export RESTIC_REPOSITORY='sftp:backup@backup-pi:/srv/restic/private-cloud'
export RESTIC_PASSWORD_FILE=/etc/private-cloud/restic-password

restic snapshots                                   # 1. what's available?
restic check                                       # 2. is it intact?
restic restore latest --target /tmp/restore-check  # 3. dry test first
restic restore latest --target /                   # 4. restore in place
```

Step 3 before step 4, always. Restoring straight over a live filesystem when
the repo turns out to be broken converts one problem into two.

**Selective restore** — a single path, or a specific snapshot:

```bash
restic restore latest --target /tmp/out --include '/tank/configs'
restic restore a1b2c3d4 --target /tmp/out          # by snapshot ID
restic mount /mnt/restic                            # browse as a filesystem
```

`restic mount` is the pleasant option when you're not sure what you need — it
exposes every snapshot as a browsable FUSE tree.

**If you've lost the restic password, stop reading.** There is no recovery
mechanism, no master key, and no support channel. The repository is
mathematically inaccessible. This is why the password belongs on paper.

---

## Recovery time expectations

| Scenario | RTO (back online) | RPO (data lost) |
|---|---|---|
| Single file from snapshot | < 1 min | ≤ 15 min |
| Dataset rollback | < 5 min | ≤ 15 min |
| Postgres from ZFS snapshot | 5–15 min | ≤ 15 min |
| Postgres from nightly dump | 15–30 min | ≤ 24 h |
| Disk replacement | 10 min + 4–24 h resilver | 0 |
| Full pool from restic | 1–6 h (network-bound) | ≤ 24 h |
| Complete server rebuild | 4–24 h | ≤ 24 h |

The RPO column is the honest one. Phase 0 accepts **up to 24 hours of data
loss** in a total-pool-loss scenario, because backups run nightly. Everything
else is covered to within 15 minutes by snapshots — but snapshots share the
disks, so they don't survive losing the pool.

---

## Verify before you trust

```bash
sudo ./scripts/restore-test.sh
```

Run it now, and quarterly thereafter. Put it in the calendar with a reminder
that outlives your enthusiasm. See [phase-0-checklist.md](phase-0-checklist.md).
