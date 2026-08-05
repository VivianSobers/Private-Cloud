# Phase 2 — Storage Engine Design

**Status: in progress — slice 1 partial.** Written before any code, so the
expensive decisions get made deliberately — the same discipline that made
Phase 1's schema survive first contact. See §7 for what is actually built.

**Exit criterion:** you can recover a file you overwrote three weeks ago, send
someone a link to a folder without giving them an account, and the pool holds
noticeably more than the sum of the bytes you uploaded. Not "chunking works" —
the storage engine actually earns its complexity.

---

## 0. What gates the start

Phase 1 is complete and verified. Two things must be true before Phase 2 writes
data in the new format:

- [ ] `sudo ./scripts/restore-test.sh` passes against a pool containing real
      Phase 1 blobs — the Phase 0 exit gate, now with something to lose
- [ ] `scripts/restic-backup.sh` includes `tank/blobs`. It was deliberately
      excluded in Phase 0 while the dataset was empty; it is not empty now, and
      Phase 2 is about to rewrite everything in it

The second is not optional. A storage migration is exactly when you want a
backup taken immediately beforehand, and the current script would silently not
have one.

---

## 1. Scope

### In

| | |
|---|---|
| **CAS engine** | FastCDC chunking, BLAKE3 addressing, zstd compression |
| **Dedup** | Chunk-level, across all files and all users |
| **Versioning** | Real history: list, restore, retention policy |
| **Share links** | Public read-only links on a separate, minimal surface |

### Explicitly out

Sync engine (Phase 3), thumbnails, OCR, semantic search, OIDC, multi-node
storage, encryption at rest beyond ZFS, per-folder ACLs.

Thumbnails and Loki were both mentioned as "Phase 2" in passing during Phase 1.
They are **not** in this phase. Thumbnails need a job queue, a job queue needs a
reason to exist, and neither serves the exit criterion. Deferring them keeps
this phase about one thing: the storage engine.

---

## 2. The shape the migration takes

Phase 1 built the indirection this phase cashes in:

```
Phase 1:  node ──► file_version ──► blob      (whole file, one row)
Phase 2:  node ──► file_version ──► manifest ──► chunks[]
                   ^^^^^^^^^^^^ unchanged
```

`file_versions` keeps its identity, its foreign keys, and its place in the API.
What changes is what it points at. That is a data migration, not a schema
rewrite over live data — which is the entire reason the table exists a phase
before it was needed.

**Both storage formats must coexist.** A migration that requires rewriting every
blob before the server can start is a migration you cannot roll back and cannot
run incrementally. So a version points at *either* a blob or a manifest, and the
reader handles both. Old files migrate in the background, or never — a file that
is never touched again is not costing anything by staying whole.

---

## 3. Chunking parameters

| Parameter | Value | Why |
|---|---|---|
| Algorithm | FastCDC | Content-defined, so an insert at the head of a file shifts one chunk boundary rather than all of them. Fixed-size blocks would dedup nothing after a one-byte prepend. |
| Min chunk | 2 KiB | Below this the per-chunk row costs more than the chunk saves |
| Target chunk | 16 KiB | |
| Max chunk | 64 KiB | |
| Hash | BLAKE3-256 | Faster than SHA-256 by a wide margin, and the speed is load-bearing: every byte written and every byte verified goes through it |
| Compression | zstd level 3 | Already an indirect dependency via Prometheus. Level 3 is the knee of the curve; higher levels cost CPU that a single box does not have spare during an upload |

**Small files bypass chunking entirely.** Below the minimum chunk size there is
nothing to divide, and a manifest row plus a chunk row to describe 900 bytes is
pure overhead. Those stay whole-file blobs, permanently.

**Already-compressed content skips zstd.** Trying to compress a JPEG, an MP4 or
a `.zst` wastes CPU to produce something marginally larger. Detected by MIME and
by a cheap entropy check on the first chunk, stored per chunk so the reader
knows without guessing.

---

## 4. Reference counting is the dangerous part

Phase 1's blob refcount is maintained by a trigger, because `ON DELETE CASCADE`
destroys version rows that no Go code ever names. Chunks make this sharper:

- One chunk is referenced by many manifests, across many files, across many
  **users**. Deleting a chunk because one user emptied their trash would corrupt
  another user's file.
- An undercount is unrecoverable data loss. An overcount is wasted disk. These
  are not symmetric, and every design choice here resolves toward the overcount.

So: the same trigger discipline, plus a **verify-before-delete** step in GC that
re-checks the refcount inside the deleting transaction, plus `fsck` gaining the
ability to *recompute* refcounts from scratch and report drift without acting on
it.

Dedup also means **quota accounting stops matching disk usage**. A user's quota
must keep counting logical bytes — the size of their files as they understand
them — not their share of physical chunks. Charging users less because someone
else happens to own the same file would be unpredictable and would leak the
existence of other users' content.

---

## 5. Versioning

`file_versions` already stores every version; Phase 1 simply never exposed them
and never kept more than the head. Phase 2 makes history real:

- Keep the last N versions and anything newer than a retention window,
  configurable, defaulting to something forgiving — with CAS, an unchanged file
  saved ten times costs ten manifest rows and no chunks.
- Restore is a **new version whose content matches an old one**, never a
  deletion of the versions in between. Destroying history to undo a mistake is
  how you turn one mistake into two.
- Version pruning must go through the same refcount path as everything else.

---

## 6. Share links

The first thing in this system reachable without an account, so it gets its own
surface and its own rules:

- A **separate Caddy site block**, as promised in the Phase 0 Caddyfile. Not a
  route on the tailnet plane. Different exposure, different rate limits,
  different logging.
- Read-only. No upload-to-share, no public WebDAV.
- Token is high-entropy and unguessable; optional password (argon2id, like
  everything else); optional expiry; optional download cap.
- Revocable, and revocation is immediate — a row, not a signed token.
- Serving a share must not leak the owner's identity, the file's path within
  their tree, or anything about the rest of their storage.

The security posture is the interesting part, and it is where this phase's
review effort should concentrate.

---

## 7. Slices

Same discipline as Phase 1: each slice ends green, committed, and useful.

| Slice | Contents | Status |
|---|---|---|
| **1a** | Chunk store: FastCDC + BLAKE3 + zstd behind `blob.Store`; `chunks` and `manifest_chunks` schema; writes chunk, reads reassemble; both formats coexist | ✅ `internal/cas` |
| **1b** | Route the upload path through the chunker; quota still counts logical bytes | ✅ all three write paths (direct, resumable, WebDAV) converge on `Service.FinishStaged`/`uploadViaCAS`; files ≥ 2 KiB chunk, smaller stay whole-file blobs permanently; identical uploads reuse a live manifest; downloads reassemble with seek |
| **2** | Chunk GC, refcount recomputation, `fsck` for CAS, background migration of Phase 1 blobs, dedup statistics | ✅ GC (manifests → chunks → bytes), fsck and refcount audit, `cas.Stats`, and in-place background migration of Phase 1 blobs — `cloudctl migrate-blobs` or the opt-in `PC_BLOB_MIGRATE_INTERVAL` loop |
| **3** | Version history: list, restore, retention policy, UI | ✅ list newest-first with the head flagged; restore appends a new head (history preserved); download any past version; retention prunes by count AND age during GC, never the head; UI history modal |
| **4** | Share links: public plane, tokens, expiry, rate limits, UI | ⬜ next |

**Slice 1b notes, recorded where the next reader will look:**

- The format decision lives in exactly two service functions: `FinishStaged`
  (staged bytes: resumable finish + WebDAV close) and `Upload`'s peek router
  (streaming, length unknown — it reads one threshold's worth to decide).
  Nothing else in the codebase chooses a storage format.
- `file_versions` rows are blob XOR manifest, enforced three times over: the
  schema CHECK, a guard in `Store.PutFile`, and the API emitting `sha256` vs
  `blake3` keyed on which one is set. A client that wants to verify a download
  needs to know which algorithm to run, so the key names it.
- Failed manifest-backed uploads delete only the MANIFEST row (chunk bytes may
  already be shared); reused manifests are never deleted on failure — the
  `ReusedManifest` guard, pinned by `TestQuotaCountsLogicalBytesUnderDedup`,
  which fails a duplicate upload on quota and then proves the file it
  deduplicated against still reads back intact.
- GC ordering is manifests before chunks so one pass reclaims a purged file
  all the way to its bytes: version rows → manifests → (cascade + trigger) →
  chunk rows → chunk bytes.

**Slice 2 migration notes, recorded where the next reader will look:**

- Migration is an **in-place UPDATE** of the version row — `blob_id` → NULL,
  `manifest_id` set — never a new version. "Only what it points at changes"
  (00003), so history stays untouched and the API cannot tell a migrated file
  from one uploaded straight to CAS.
- The refcount trigger from 00003 fired on INSERT/DELETE only. Migration 00008
  teaches it **UPDATE**, or the in-place switch would move the reference off the
  old blob without decrementing it, and GC — which only reclaims blobs at zero —
  would leak every migrated blob forever. This is the one subtlety that makes the
  slice work; `TestMigrateDropsOldBlobToZeroThenGC` pins it.
- Ordering is the same crash-safety contract as every write path: chunks and
  manifest durable **before** the switch, old blob left for GC rather than
  deleted inline. `Store.SwitchToManifest` guards on `blob_id = $old AND
  manifest_id IS NULL`, so a concurrent overwrite, purge, or second worker lands
  as a no-op and the freshly built manifest is dropped as an orphan.
- A version whose bytes are already gone is **failed, never repointed** — you
  cannot build a manifest from content that is missing, and a size disagreement
  aborts that one file rather than the whole pass. Those surface through `fsck`.
- Operator-driven by default (`cloudctl migrate-blobs [--all]`), because a
  storage rewrite is exactly when §0's backup should already exist. The
  background loop is opt-in via `PC_BLOB_MIGRATE_INTERVAL`.

**Slice 2's checker landed before slice 1b on purpose.** `fsck` walks the blob
directory and deletes anything the database does not name. Chunks live in that
same directory under the same `ab/cd/hash` layout but are recorded in `chunks`,
not `blobs` — so a checker that only understands whole-file blobs would classify
every deduplicated byte as an orphan and delete it on the first `fsck --repair`.
The checker has to understand the new format *before* anything writes it, not
after. `TestFsckDoesNotTreatChunksAsOrphans` exists to keep it that way.

---

## 8. Risks

| Risk | Mitigation |
|---|---|
| Chunk refcount undercount deletes live data | Trigger-maintained, verified inside the deleting transaction, recomputable by `fsck` |
| Migration cannot be rolled back | Both formats coexist; migration is incremental and interruptible; old blob is deleted only after the manifest verifies |
| Reassembly is slower than reading a file | Measure before optimising. Range requests need chunk-offset indexing, which is the one place a naive implementation will be visibly bad |
| Share links leak more than intended | Separate surface, no owner metadata in responses, reviewed as its own slice |
| Dedup makes quota confusing | Quota counts logical bytes, always |
| CPU cost of hashing and compressing on a small box | BLAKE3 over SHA-256; zstd level 3; skip compression for incompressible content |
