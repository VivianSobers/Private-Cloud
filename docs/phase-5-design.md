# Phase 5 — Photos & Media Design

**Status: partial, and unevenly so.** The front-of-API half is finished; the
behind-the-API half is built up to the point where it would become reachable and
stops there. Written after the fact rather than before it — which is itself the
finding: every other phase in this project got a design document first, this one
did not, and it is the phase whose two halves drifted apart.

**Exit criterion:** a person opens Photos and sees their pictures as a
date-ordered grid that loads in tiles rather than in originals, opens one to a
full-screen preview, and groups a selection into an album that orders how they
dragged it and moves nothing on disk.

**Where it actually stands:** the schema, the decoding/rendering package and the
metadata store are built and tested. Nothing enqueues a media job, no worker
registers the handler, and **no HTTP route serves any of it** — so the finished
gallery in `web/` talks to endpoints that answer `404`. See §7.

---

## 0. The split, and how it came apart

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
| **5** | **The job handler and its worker registration** | ⬜ **not landed.** Needs: a `media` job handler; the `files`↔`media` adapters (opener, blob writer, variant store); `runner.Register(media.Kind, …)` in `pcworker`; and an enqueue on upload beside the existing `extract` enqueue |
| **6** | **`?variant=` on the content route** | ⬜ **not landed.** No route reads the parameter today |
| **7** | **`GET /media/timeline`** | ⬜ **not landed** |
| **8** | **`/albums` CRUD + items + reorder** | ⬜ **not landed.** The tables have no Go code touching them at all |
| **9** | Gallery, timeline, lightbox, album views in `web/` | ✅ built against the contract; currently degrade to "not available on this server yet" |
| 10 | Drag-reorder and "add to album" wiring | ⬜ blocked on slice 8 |
| 11 | Map view from EXIF GPS | ⬜ blocked on slice 7 |

**Slices 5–8 are the whole remaining gap, and they are what make everything
already built reachable.** Nothing in 1–4 is observable from outside the process
until 5 runs and 6–8 serve it.

## 8. Risks

- **A backfill is required, not optional.** Every file uploaded before slice 5
  lands has no media row, and the timeline is empty for exactly the library that
  motivated the feature. `cloudctl jobs reindex` already exists for the
  extraction/embedding case and is the right lever; it needs the `media` kind
  added rather than a new mechanism.
- **Variant bytes are not yet visible to `fsck` or GC.** `media_variant` has the
  `storage_key` index that makes the reference check cheap, but the walker has to
  learn to consult it. Until it does, a variant looks like an orphan blob to a
  repair pass — the one direction that can delete live data.
- **Video is honest about being unfinished.** `analyzeVideo` records that a file
  *is* video and nothing else. Duration, dimensions and rotation live in MP4/MKV
  boxes that need a real demuxer, and both honest options — a cgo dependency on
  ffmpeg, or shelling out to it — belong behind the same "is this deployment set
  up for it" switch OCR already sits behind, not silently inside a pure package.
  A video therefore orders by upload time in the timeline until that lands.
