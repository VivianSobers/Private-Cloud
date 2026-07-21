-- +goose Up
-- Phase 2, slice 1: content-addressed storage.
--
-- The indirection Phase 1 built now pays for itself:
--
--     Phase 1:  node -> file_version -> blob            (whole file)
--     Phase 2:  node -> file_version -> manifest -> chunks[]
--
-- file_versions keeps its identity, its foreign keys and its place in the API.
-- Only what it points at changes.
--
-- BOTH FORMATS COEXIST, permanently. A migration that must rewrite every blob
-- before the server starts is one you cannot roll back and cannot run
-- incrementally. A file nobody touches again costs nothing by staying whole.

-- ---------------------------------------------------------------------------
-- chunks — content-addressed, deduplicated across all files and all users
-- ---------------------------------------------------------------------------
CREATE TABLE chunks (
    -- BLAKE3-256 of the PLAINTEXT chunk. The address is the content, which is
    -- what makes dedup free: two users uploading the same file converge on the
    -- same rows without either of them knowing.
    --
    -- Hashing plaintext rather than the compressed form matters: compression
    -- output can vary with library version or settings, and addressing by it
    -- would silently stop deduplicating after a dependency upgrade.
    hash bytea PRIMARY KEY CHECK (octet_length(hash) = 32),

    -- Logical size. What the chunk occupies in the reassembled file.
    size integer NOT NULL CHECK (size > 0),

    -- Physical size on disk, after compression. Differs from size only when
    -- compression is not 'none'.
    stored_size integer NOT NULL CHECK (stored_size > 0),

    -- 'none' or 'zstd'. Stored per chunk rather than assumed globally, so the
    -- reader never has to guess and incompressible content can skip zstd
    -- without a separate lookup table.
    compression text NOT NULL DEFAULT 'none' CHECK (compression IN ('none', 'zstd')),

    -- Path within the blob store. Derived from the hash, but stored explicitly
    -- so the layout can change without rewriting the table.
    storage_key text NOT NULL,

    -- References from manifest_chunks. bigint, not integer: a chunk of zeroes
    -- inside a few large sparse files will exceed 2^31 references long before
    -- anything else in this schema strains.
    refcount bigint NOT NULL DEFAULT 0 CHECK (refcount >= 0),

    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX chunks_storage_key_key ON chunks (storage_key);
CREATE INDEX chunks_gc_idx ON chunks (created_at) WHERE refcount = 0;

-- ---------------------------------------------------------------------------
-- manifests — the ordered chunk list that reconstitutes one file version
-- ---------------------------------------------------------------------------
CREATE TABLE manifests (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Total logical bytes, denormalised from the chunk list. A Range request
    -- needs the file length before it needs any chunk, and summing the chunks
    -- to answer HEAD would be absurd.
    total_size bigint NOT NULL CHECK (total_size >= 0),
    chunk_count integer NOT NULL CHECK (chunk_count >= 0),

    -- BLAKE3 of the whole reassembled file. Phase 1 stored SHA-256 on the blob;
    -- keeping a whole-file digest here preserves the strong download ETag and
    -- gives fsck something to verify reassembly against.
    content_hash bytea NOT NULL CHECK (octet_length(content_hash) = 32),

    created_at timestamptz NOT NULL DEFAULT now()
);

-- Whole-file dedup: an identical upload finds an existing manifest instead of
-- rebuilding one. Not unique — two manifests with the same content are
-- harmless, and enforcing uniqueness would turn a race between two identical
-- uploads into an error for one of them.
CREATE INDEX manifests_content_hash_idx ON manifests (content_hash);

CREATE TABLE manifest_chunks (
    manifest_id uuid NOT NULL REFERENCES manifests (id) ON DELETE CASCADE,

    -- Position in the file. Chunks are not unique within a manifest — a file
    -- of repeated content references the same chunk many times — so the key is
    -- (manifest, position), never (manifest, chunk).
    seq integer NOT NULL,

    -- RESTRICT, not CASCADE: deleting a chunk out from under a live manifest
    -- would leave a file with a hole in the middle. GC must clear the
    -- reference first, and this constraint is what forces that ordering.
    chunk_hash bytea NOT NULL REFERENCES chunks (hash) ON DELETE RESTRICT,

    -- Byte offset of this chunk within the reassembled file. Denormalised so a
    -- Range request can find its starting chunk with an index lookup instead of
    -- summing every preceding chunk — the one place a naive implementation is
    -- visibly bad, and the reason video scrubbing works at all.
    byte_offset bigint NOT NULL CHECK (byte_offset >= 0),

    PRIMARY KEY (manifest_id, seq)
);

-- Serving a Range request: "the chunk covering byte N of this manifest".
CREATE INDEX manifest_chunks_offset_idx ON manifest_chunks (manifest_id, byte_offset);
-- GC and fsck: "what still references this chunk".
CREATE INDEX manifest_chunks_hash_idx ON manifest_chunks (chunk_hash);

-- ---------------------------------------------------------------------------
-- file_versions points at ONE OF blob or manifest
-- ---------------------------------------------------------------------------
ALTER TABLE file_versions ALTER COLUMN blob_id DROP NOT NULL;

ALTER TABLE file_versions
    ADD COLUMN manifest_id uuid REFERENCES manifests (id) ON DELETE RESTRICT;

-- Exactly one, never both, never neither. Catching this in the schema means no
-- application bug can produce a version that two different readers disagree
-- about how to read.
ALTER TABLE file_versions
    ADD CONSTRAINT file_versions_storage_check
    CHECK ((blob_id IS NULL) <> (manifest_id IS NULL));

CREATE INDEX file_versions_manifest_idx ON file_versions (manifest_id)
    WHERE manifest_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- refcount maintenance
-- ---------------------------------------------------------------------------
-- A trigger, for the same reason blobs use one: manifest_chunks rows disappear
-- through ON DELETE CASCADE when a manifest is dropped, and no Go code ever
-- names them. Service-layer accounting would have to reimplement the cascade
-- to know what it just destroyed.
--
-- The stakes are higher here than for blobs. One chunk is referenced across
-- many files and many USERS, so an undercount does not lose one person's data
-- — it corrupts a file belonging to someone who was never involved.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION chunk_refcount_bump() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE chunks SET refcount = refcount + 1 WHERE hash = NEW.chunk_hash;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE chunks SET refcount = refcount - 1 WHERE hash = OLD.chunk_hash;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER manifest_chunks_refcount
    AFTER INSERT OR DELETE ON manifest_chunks
    FOR EACH ROW EXECUTE FUNCTION chunk_refcount_bump();

-- +goose Down
-- The refusal comes FIRST, before anything is dropped. Rolling this migration
-- back destroys the only record of how a manifest-backed version is assembled,
-- and a file that cannot be reassembled is a lost file — not something to
-- discover halfway through a DDL script. goose wraps each migration in a
-- transaction so a later RAISE would also roll back, but relying on that means
-- the safety depends on the runner rather than on the migration.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM file_versions WHERE manifest_id IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back: % file version(s) are stored as manifests. Migrate them to whole-file blobs first.',
            (SELECT count(*) FROM file_versions WHERE manifest_id IS NOT NULL);
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS manifest_chunks_refcount ON manifest_chunks;
DROP FUNCTION IF EXISTS chunk_refcount_bump();

ALTER TABLE file_versions DROP CONSTRAINT IF EXISTS file_versions_storage_check;
DROP INDEX IF EXISTS file_versions_manifest_idx;
ALTER TABLE file_versions DROP COLUMN IF EXISTS manifest_id;

ALTER TABLE file_versions ALTER COLUMN blob_id SET NOT NULL;

DROP TABLE IF EXISTS manifest_chunks;
DROP TABLE IF EXISTS manifests;
DROP TABLE IF EXISTS chunks;
