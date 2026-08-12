# Phase 9 — Scale & resilience design

**Status: partial, and deliberately so.** The observability and quota halves are
built. **The object-storage cold tier is not**, and this document says why rather
than leaving a roadmap row implying otherwise.

---

## 0. What shipped

**`GET /admin/storage`** — pool health, backup freshness, accounted bytes and
queue depth, admin only.

The contract's rule for it is the whole design, and it is followed literally:
read from the **same sources the alerts already use** — the zpool textfile
collector, restic's success timestamp, the jobs table — rather than inventing a
second, parallel notion of health. Two notions disagree eventually, and then
nobody knows which to believe at the moment it matters.

So nothing here runs `zpool status` or `restic snapshots`. The API process does
not shell out to storage tooling; it reads the files
[scripts/zpool-metrics.sh](../scripts/zpool-metrics.sh) and
[scripts/restic-backup.sh](../scripts/restic-backup.sh) already write, which
means the admin console and Grafana are looking at the same numbers by
construction.

A hand-rolled parser rather than a Prometheus exposition library: this reads two
files this repository writes, in a format it controls, and pulling in a
dependency to read four metric names would be a dependency for nothing.

### Distinctions the report is careful about

- **Never scrubbed is not the same as failed.** `last_scrub_clean` is absent when
  a pool has never been scrubbed, and `false` only when a scrub ran and found
  errors. Collapsing them reports a brand-new pool as damaged.
- **A stale report is not a healthy pool.** `collected_at` is included so an
  operator can tell a live "ONLINE" from one written a week ago by a collector
  that has since stopped.
- **No collector is not an empty pool.** `collector.available` distinguishes "we
  could not read the textfile directory" — the ordinary case on a dev box — from
  "the collector ran and found nothing", and `collector.path` says where the
  server looked.
- **Accounted bytes are not pool capacity.** The database knows what the
  application stored; the collector knows what the disks hold. Conflating them
  produces a number that is wrong in both readings, so the response reports the
  former under `accounted` and does not claim the latter.
- **A malformed line costs one metric, not the report.**

**Quota enforcement** was built in Phase 1 and made settable by an administrator
in Phase 7. What Phase 9 owed was proof the two meet, which is now tested:
refusal is `507 quota_exceeded` (a storage condition, and WebDAV clients already
read 507 as "stop, you are full"); trashed bytes still count, because they really
are still on the disk; purging frees them; clearing a quota restores unlimited;
and — the Phase 7 interaction — an **editor writing into a shared folder spends
the owner's quota**, not their own.

## 1. What did not ship, and why it is not a stub

**The object-storage cold tier is not implemented.** Neither is a tiering policy.

`GET /admin/storage` reports `tiering: {enabled: false}` with a note, rather than
a `cold` tier holding zero bytes. A zero-byte cold tier would imply the feature
exists and is merely empty, which is a more misleading answer than saying it is
absent.

This is the one genuinely large piece of the original roadmap left undone, and it
is left undone honestly rather than half-built, because a half-built tier is the
worst possible state for a storage system: content that has been "moved to cold"
by code that cannot reliably read it back is content that is gone.

What it would take, sketched so the next person does not start from nothing:

- A second `blob.Store` implementation over an S3-compatible API, sitting beside
  `FSStore` behind the interface that already exists.
- A `tier` column on `blobs` and `chunks`, and a policy job that demotes content
  by age and access recency.
- A read path that transparently promotes on access, and a **restore-before-read
  latency contract** the download handler can express — object storage retrieval
  is not disk retrieval, and pretending otherwise makes downloads hang.
- `fsck` taught about a third location, which is exactly the trap this codebase
  has already fallen into twice: once when chunks arrived and once when media
  variants did, both times leaving `--repair` willing to delete live data.

That last point is the reason not to rush it.

## 2. Slices

| # | Slice | Status |
|---|---|---|
| **1** | `GET /admin/storage` from the existing collectors | ✅ 5 parser tests |
| **2** | Quota enforcement end to end, including owner-charged editor writes | ✅ 6 tests |
| 3 | Object-storage cold tier + tiering policy | ⬜ **not started** — see §1 |
| 4 | DR automation / restore drills as code | ⬜ the runbooks and `scripts/restore-test.sh` exist and are manual; automating them is not started |
| 5 | Billing hooks | ⬜ not started, and arguably out of scope for a self-hosted single-tenant server |

## 3. Risks

- **`/admin/storage` is only as good as the collectors.** If the systemd timers
  that write those `.prom` files are not installed, the endpoint honestly reports
  `collector.available: false` — but an operator who does not read that field
  will see an empty pool list and may conclude the pool is fine. The Phase 0
  checklist is what installs them.
- **Quota is charged to the owner and an editor can exhaust it.** Correct owner
  for the bytes, but nothing bounds how much an editor may add; the owner's
  remedy is revocation after the fact. Carried over from Phase 7 and still open.
- **Per-user API rate limiting remains deferred**, now through five phases. It
  was acceptable when the system was single-user; with sharing, chat and
  similarity all live, one authenticated user can now cost a GPU box real work.
  This is the most overdue item in the project.
