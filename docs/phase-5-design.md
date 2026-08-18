# Phase 5 — Photos & Media Design

**Status: 🟠 server complete (8/8 slices); one front-of-API slice open.** The exit
criterion below is met and the phase is usable end to end — and the photo viewer has
since grown Phase 8's face overlay and find-similar, and Phase 6's offline pinning,
on top of it.

Still ❌: a **map view** over the EXIF GPS the node already serves, and video metadata
beyond "this is a video", which needs a demuxer. Still 🟠: **reordering an album by
pointer drag** — adding to an album and replacing the whole order in one call both
work, but a person reorders with move-up/move-down buttons.

Marks: ✅ done · 🟠 partial · ❌ not built; the ledger is [status.md](status.md).

Written after the code rather than before it — which is
itself part of the record: every other phase in this project got a design
document first, this one did not, and it is the phase whose two halves drifted
furthest apart before they were joined.

**Exit criterion:** a person opens Photos and sees their pictures as a
date-ordered grid that loads in tiles rather than in originals, opens one to a
full-screen preview, and groups a selection into an album that orders how they
dragged it and moves nothing on disk. **Met.**

The first revision of this document recorded the phase as half-built: the schema,
the decoding package and the store existed, nothing enqueued a media job, no
worker registered the handler, and no route served any of it, so the finished
gallery in `web/` talked to endpoints that answered `404`. Slices 5–8 closed
that; the sections below describe the shipped design, and §7 records what each
slice actually is.

---

## 0. The split, and how it came apart — and back together

Per [roadmap-split.md](roadmap-split.md) this phase divides at the API seam:
thumbnail/EXIF jobs, album endpoints and variant serving behind it; gallery,
lightbox and album UI in front of it. The seam worked as designed for the
*contract* — the endpoint shapes in [api-contract.md](api-contract.md) were
specified first, and the web client coded against them exactly, with no contact
between the two tracks.

What was missing was any check that the shapes were ever implemented. The front
track finished and the back track did not, and because the contract deliberately
documents proposed endpoints alongside shipped ones, nothing in the repository
disagreed with anything else. That gap is now closed by
[openapi.yaml](openapi.yaml) and the contract test, which is generated from the
server's real route table and fails when the two disagree. This document exists
for the same reason: a phase should say what it has not done.

## 1. Everything derived is content-addressed

`media_meta` and `media_variant` are both keyed by **content hash**, not by node
id — the same choice `doc_text` and `doc_embedding` made in Phase 4, for the same
reason. A photo's dimensions, its capture time and its thumbnail are properties
of the *bytes*. The same picture uploaded twice, or shared into two accounts, is
decoded and resized once.

On this hardware that is not a micro-optimisation. Decoding a 24-megapixel JPEG
and rescaling it twice costs seconds of CPU on a box with one spare core, and a
phone's camera roll is full of files that arrive more than once.

It also means the work survives the file. Purging and re-uploading the same photo
costs nothing the second time, and the CAS refcounting that already governs
chunks governs the variant bytes too.

## 2. `taken_at` is the point of the table

The one column that justifies the whole schema is `taken_at`: **when the shutter
fired, not when the file was uploaded.** A photo copied off a camera in 2026 was
still taken in 2019, and a timeline that sorts by `created_at` is a timeline of
your file transfers, not of your life.

It is nullable, and the fallback to `updated_at` is deliberately left to the
client rather than materialised server-side, so the fallback stays *visible* — a
UI can show "date unknown" instead of confidently displaying an import date as a
capture date.

Dimensions are stored **as encoded**, before the orientation flag is applied. A
client that rotates by `orientation` must swap width and height itself for the
quarter-turn cases. Recording them post-rotation would silently disagree with
what every decoder reports for the same file.

GPS is stored exact and unrounded. This is a private server: the owner already
holds the original file with the same EXIF inside it, so blurring here would
protect nothing and break a map view.

## 3. Variants are a parameter, not a new route

```
GET /nodes/{id}/content?variant=thumb|preview|original    (default: original)
```

Not `GET /nodes/{id}/thumb`. Reusing the existing content route means range
requests, ETags, `Cache-Control`, the download disposition and the **public share
plane** all keep working for variants with no new code and no second security
review of a second byte-serving path.

Two fixed sizes, not a free-form `?w=`. An arbitrary width means rendering on
demand, which puts image decoding on the request path of the always-on box —
precisely what the job queue exists to keep off it. 320px covers a grid tile at
2x on a phone; 1600px covers a lightbox on a 1440p display.

A variant that has not been rendered yet is an honest `404 variant_unavailable`,
never a silent fallback to the original. A gallery of 200 tiles quietly serving
12 MB originals would pull gigabytes and look like a network problem rather than
a missing job.

## 4. An album is not a folder

- A node lives in exactly one folder. It can be in many albums.
- Adding to an album does not move the file; removing from one does not delete
  it.
- Albums are ordered by hand; folders are ordered by name.

Modelling an album as a folder would mean either moving files — breaking the sync
client's view of the tree, and every device's — or copying them, breaking dedup
and charging quota twice. A join table is the honest shape.

Ordering is replaced wholesale by `PATCH /albums/{id}/items`, not patched per
item: a drag-reorder that issues N position updates is N chances to end up
half-applied. Adding a node already present is a no-op rather than an error,
which is what makes a retried request safe — the primary key does that work.

`DELETE /albums/{id}` never touches file content. Worth stating in the contract
because it is the question every user has before they click it.

## 5. A third job kind, not a bigger extractor

Media is its own `kind` on the existing queue, alongside `extract` and `embed`,
so the three can be placed on different machines independently: media work needs
file bytes and CPU but no model, extraction needs bytes and tesseract, embedding
needs a sidecar and no bytes at all.

The handler is idempotent by content hash and checks for existing metadata
*before* reading the file, so a re-upload of a photo already in the library costs
one query rather than a decode.

Metadata is stored **before** variants are rendered, and a rendering failure is
logged rather than returned. The order matters: dimensions and capture time are
what put a photo in the timeline at all, while a missing thumbnail only degrades
one tile. Failing the job after the metadata write would also mean redoing the
decode on retry for a file that is already usable.

## 6. Decoding is the hostile-input surface

This is the only place in the system that parses attacker-supplied binary
formats in-process, so the bounds are explicit:

- **`MaxInputBytes` = 40 MiB**, lower than the extractor's 64 MiB, because
  decoding is not streaming.
- **`MaxPixels` = 80,000,000**, checked against the header via
  `image.DecodeConfig` **before** anything decodes. This is the real guard: a
  100-megapixel PNG can be a few hundred kilobytes of highly compressible black
  and 400 MB of decoded RGBA, which on a 7 GiB box is the whole worker. File size
  is a poor proxy for decode cost; pixel count is the honest one.
- Formats are an **allowlist** (`image/jpeg`, `image/png`, `image/gif`), not an
  `image/*` prefix test — SVG is `image/*` and is not a raster image at all.
- A file that claims to be an image and is not is a *completed* job, not a
  retried one. It will not get better on the fifth attempt, and burning the
  backoff schedule on it delays real work.

## 7. Slices

| # | Slice | Status |
|---|---|---|
| **1** | Media schema: content-addressed `media_meta` + `media_variant`, timeline index (`00019`) | ✅ |
| **2** | Album schema: `albums` + `album_items`, hand ordering, no-op re-add (`00020`) | ✅ |
| **3** | The `media` package: EXIF reader, `Analyze`, thumbnail/preview renderer, bomb guards | ✅ pure of the database and the queue, tested on synthetic images |
| **4** | The metadata + variant store (`files/media.go`): read/write meta, variants, `MediaVariantFor` | ✅ integration-tested |
| **5** | The job handler, its `files`↔`media` adapters, worker registration and the enqueue on upload | ✅ MIME-filtered at the enqueue; end-to-end tested from upload to rendered thumbnail |
| **6** | `?variant=thumb\|preview` on the content route | ✅ `404 variant_unavailable` when not yet rendered; immutable caching |
| **7** | `GET /media/timeline` | ✅ sorted by capture time, metadata batched per page |
| **8** | `/albums` CRUD + items + reorder | ✅ ownership enforced on every node id the caller supplies |
| **9** | Gallery, timeline, lightbox, album views in `web/` | ✅ built against the contract; they lit up unchanged when the endpoints started answering, and the lightbox is now where face correction, find-similar and offline pinning live too |
| 10 | Reorder and "add to album" wiring in `web/` | 🟠 **shipped, but not as a drag.** Selection mode → "Add to album…" ✅ (`POST /albums/{id}/items`, one call for the whole selection); reordering ✅ via move-up/move-down buttons in a "Manage" mode, persisted with the wholesale `PATCH /albums/{id}/items`. ❌ No pointer drag and no drag-select. The endpoint contract — replace the order wholesale, never N per-item updates — was written for a drag and the buttons already satisfy it, so the remaining work is entirely in the browser |
| 11 | Map view from EXIF GPS | ❌ **not built.** `gps` is served on the node, unrounded, and typed in the web client (`api.ts`) — nothing renders it. No map library is a dependency, which is the decision to make first: an offline-capable PWA and a tile provider that phones home are in tension |

## 8. What the risks turned into

The first revision of this document listed three risks. Two are closed and the
third stands.

- **The backfill was required, not optional — and is now available.** Every file
  uploaded before the media kind existed has no metadata row, and the timeline
  selects on that row, so without a backfill the gallery is empty for exactly the
  library that motivated the feature. `cloudctl jobs reindex --kind=media`
  enqueues it, filtered to image and video MIME types so the pass is proportional
  to the number of photos rather than to the size of the tree.
- **Variant bytes are now visible to `fsck` and GC.** They were not, and that was
  the dangerous one: variant bytes go through the ordinary blob `Put`, so they
  share a root and a layout with blobs and chunks but are referenced from
  `media_variant` instead of `blobs`. `fsck` classified every thumbnail as an
  orphan, and `--repair` would have deleted the entire rendered set — the same
  failure the function's own doc comment records for chunks, one storage format
  later. `Fsck` now loads `MediaVariantKeys` and accounts for them, reporting a
  variant whose bytes are gone under `MissingVariants` rather than `Missing`,
  because a variant is derived and a reindex rebuilds it: that report must not
  send an operator to a backup. GC reclaims unreferenced variants row-first, then
  bytes, the same ordering blob and manifest collection use.
- **Video is still honest about being unfinished.** `analyzeVideo` records that a file
  *is* video and nothing else. Duration, dimensions and rotation live in MP4/MKV
  boxes that need a real demuxer, and both honest options — a cgo dependency on
  ffmpeg, or shelling out to it — belong behind the same "is this deployment set
  up for it" switch OCR already sits behind, not silently inside a pure package.
  A video therefore orders by upload time in the timeline until that lands.
