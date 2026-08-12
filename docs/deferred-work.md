# Deferred work

Everything this system deliberately does not do, in one place, with the reason.

This exists because "not built yet" and "decided against" look identical from
outside the code, and both look identical to "nobody remembered". Those three
need very different responses, and the answer for each was previously scattered
across nine design documents — findable only by someone who already knew it was
there, which is exactly the person who does not need to look.

The rule: anything absent on purpose gets a line here. If it is not here and it
is not built, that is a bug, not a decision.

Two of these are enforced mechanically rather than by this file. Routes with no
client are declared in `awaitingClient` in `server/internal/httpapi/contract_test.go`,
which fails when one is neither consumed nor declared. Everything else below is
a judgement, and a judgement can only be written down.

---

## Not built, and deliberately so

### Object-storage cold tier (Phase 9)

`GET /api/v1/admin/storage` reports `tiering: {enabled: false}` rather than a
cold tier holding zero bytes, because a zero-byte tier would imply the feature
exists and is merely empty.

**Why not:** a half-built storage tier is the worst state this system can be in.
Content "moved to cold" by code that cannot reliably read it back is content that
is gone, and it is gone silently, and it is gone for the files least recently
touched — the ones nobody will notice for months. It also needs fsck taught about
a third storage location, which is a trap this codebase has already fallen into
twice: once when chunks arrived and `--repair` would have deleted every
deduplicated byte, and again when media variants arrived and it would have
deleted every thumbnail.

**Before starting it:** fsck accounts for the third location, and the read path
is proven against a cold store that is slow and occasionally unavailable, before
a single byte moves.

See `docs/phase-9-design.md` §1.

### Disaster-recovery automation (Phase 9)

Restore is documented and rehearsed by hand — `docs/runbook-restore.md`,
`docs/runbook-disaster-recovery.md`, `scripts/restore-test.sh`.

**Why not:** the `sudo` gates in the restore path are the operator's to tick.
Automating a restore means automating something whose failure mode is
overwriting good data with old data, and the rehearsal is worth more than the
automation until the rehearsal is boring.

### Billing hooks (Phase 9)

**Why not:** there is no second tenant. Quota exists and is enforced; the thing
billing would attach to is one person's disk.

### Streaming chat answers (Phase 8)

`POST /api/v1/chat` returns a complete answer or none.

**Why not:** citations are computed from the retrieved passages and are mandatory
— an answer that streamed ahead of its citations would be, for the duration of
the stream, exactly the unverifiable output this design refuses to produce.
Streaming is worth having and needs the citation contract solved first, not
after.

### Image-embedding similarity for photos (Phase 8)

`/similar` works on documents, through the text-embedding space Phase 4 built.
Photos have no text and so have no neighbours.

**Why not:** it needs a second model and a second vector space, and the value is
mostly already delivered by face clustering and the timeline. Worth doing;
nothing depends on it.

### Video metadata beyond "this is a video" (Phase 5)

`analyzeVideo` records that a file is video and nothing else. No duration, no
dimensions, no rotation, no thumbnail.

**Why not:** those live in MP4/MKV boxes that need a real demuxer, and the honest
options are a cgo dependency on ffmpeg or shelling out to it. Both belong behind
the same "is this deployment set up for it" switch OCR sits behind, not silently
inside the media package.

**Consequence to know about:** a video appears in the timeline ordered by upload
time rather than capture time, and its tile has no thumbnail.

### Face detection on by default (Phase 8)

`cloudctl jobs reindex --kind=faces` is opt-in and is not part of `--kind=all`.

**Why not:** it needs a detector sidecar most deployments will not run. Queueing
a job per photo on a server with no detector fills the dead-letter queue instead
of doing anything.

---

## Known limits worth stating

### The chunk-existence oracle is closed at the cost of cross-user transfer dedup

`POST /api/v1/chunks/have` answers from the caller's own chunks only, so a
stranger is told to upload content the server already holds. Storage dedup is
unaffected — `PutKeyed` is a no-op for a key that exists — but the transfer is
paid twice.

**Why:** the global answer was a truthful yes/no about whether any given content
exists on this server, for anyone who could guess the bytes. That is a real
oracle, and bandwidth is the cheaper thing to spend.

### Quota counts logical bytes, deduplicated, not blocks on disk

`Usage.TotalBytes` counts each distinct content once across live, trashed and
retained versions.

**Why:** it is the number a person can predict from what they uploaded. Actual
disk depends on compression ratios and on content shared with other accounts, and
charging someone for a chunk they share with a stranger is not explicable.

**Consequence:** the sum of every account's usage does not equal the pool's used
bytes, and should not be expected to.

### Clustering is greedy and order-dependent (Phase 8)

A person photographed over years may end up in several clusters.

**Why:** re-partitioning globally on each arrival would either run constantly or
keep renaming clusters somebody has already named. A name is a promise. Merge
and reassign are the correction path, and corrections are now permanent —
`faces.dismissed_at`, migration 00024.

### Rate limiting is in-process (Phase 4)

Correct for a single node. If the API is ever replicated, the limiter has to move
to shared state; until then an external store would add a dependency to the auth
path in exchange for a property that does not exist here.

### Integration tests share one database

They do not fully isolate, and a stale database produces failures that look like
regressions — chunk GC counts on-disk chunks globally, media variants are shared
by content hash across fixtures whose blob stores are separate temp directories.

**Run them with a fresh Postgres container and `-p 1`.** Before concluding a
change broke something, recreate the container and run it again.
