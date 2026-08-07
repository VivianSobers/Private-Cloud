-- +goose Up
-- Phase 5, slice 1: media metadata and derived variants.
--
-- Both are CONTENT-ADDRESSED, exactly like doc_text and doc_embedding: what a
-- photo's dimensions are, when its shutter fired, and what its thumbnail looks
-- like are all properties of the BYTES, not of the node. The same picture
-- uploaded twice, or shared into two accounts, is decoded and resized once and
-- shares both rows.
--
-- That is not a micro-optimisation on this hardware. Decoding a 24-megapixel
-- JPEG and rescaling it twice costs seconds of CPU on a box with one spare core,
-- and a phone's camera roll is full of files that arrive more than once.

CREATE TABLE media_meta (
    content_hash bytea PRIMARY KEY,

    -- Pixel dimensions as STORED, before the orientation flag is applied. A
    -- client that rotates by `orientation` must swap these itself for the
    -- quarter-turn cases; recording them post-rotation would silently disagree
    -- with what a decoder reports for the same file.
    width  int,
    height int,

    -- EXIF orientation, 1-8. 1 is "as stored"; anything else means the camera
    -- was rotated and the viewer is expected to compensate.
    orientation int NOT NULL DEFAULT 1 CHECK (orientation BETWEEN 1 AND 8),

    -- When the shutter fired, NOT when the file was uploaded. This is what a
    -- timeline sorts by, and it is the whole reason this table exists: a photo
    -- copied off a camera in 2026 was still taken in 2019.
    taken_at timestamptz,

    camera text NOT NULL DEFAULT '',

    -- Exact coordinates, deliberately unrounded. This is a private server; the
    -- owner already has the original file with the same EXIF in it, so blurring
    -- here would protect nothing and break a map view.
    gps_lat double precision,
    gps_lon double precision,

    -- Videos only. Milliseconds, so a duration is an integer rather than a float
    -- that has to be compared with a tolerance.
    duration_ms bigint,

    -- Which decoder produced this ('image', 'video'), so a better extractor
    -- later can be told which rows are worth redoing.
    source text NOT NULL DEFAULT '',

    extracted_at timestamptz NOT NULL DEFAULT now()
);

-- The timeline's sort key. Partial, because rows without a taken_at can never
-- appear in a date-ordered view and would only pad the index.
CREATE INDEX media_meta_taken_at ON media_meta (taken_at DESC) WHERE taken_at IS NOT NULL;

-- Derived renditions: a thumbnail for a grid, a preview for a lightbox.
--
-- The bytes live in the ordinary blob store under their own content-addressed
-- key, so identical thumbnails from identical originals collapse to one file on
-- disk for free. The row is what makes them findable and reclaimable; nothing
-- else references that key, so GC deletes the row and then the file, in that
-- order, exactly as it does for blobs.
CREATE TABLE media_variant (
    content_hash bytea NOT NULL,
    variant      text  NOT NULL CHECK (variant IN ('thumb', 'preview')),

    storage_key text   NOT NULL,
    mime        text   NOT NULL,
    size        bigint NOT NULL CHECK (size > 0),
    width       int    NOT NULL CHECK (width > 0),
    height      int    NOT NULL CHECK (height > 0),

    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (content_hash, variant)
);

-- fsck walks the blob store and must be able to answer "is this key referenced?"
-- for a variant's key as cheaply as it does for a blob's.
CREATE INDEX media_variant_storage_key ON media_variant (storage_key);

-- +goose Down
DROP TABLE IF EXISTS media_variant;
DROP TABLE IF EXISTS media_meta;
