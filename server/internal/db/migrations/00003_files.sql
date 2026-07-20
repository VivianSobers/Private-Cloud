-- +goose Up
-- Phase 1, slice 3: the file tree.
--
-- The shape here is deliberately the PHASE 2 shape, even though slice 3 only
-- ever writes one version per file and stores whole files rather than chunks:
--
--     node -> file_version -> blob
--
-- Phase 2 replaces what file_version points at (a manifest of content-addressed
-- chunks instead of a whole blob). Because the versioned indirection already
-- exists, that is a data migration rather than a schema rewrite over live data.

-- ---------------------------------------------------------------------------
-- blobs — opaque stored bytes
-- ---------------------------------------------------------------------------
CREATE TABLE blobs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Path within the blob store, assigned by the storage layer. The database
    -- never constructs it; the BlobStore interface owns that mapping so it can
    -- change in Phase 2 without touching SQL.
    storage_key text   NOT NULL,

    size        bigint NOT NULL CHECK (size >= 0),

    -- Content hash, computed while streaming the upload. Phase 2 replaces this
    -- with per-chunk BLAKE3; for now it powers integrity checks in fsck.
    sha256      bytea  NOT NULL,

    -- Number of file_versions referencing this blob. Garbage collection deletes
    -- blobs that reach zero and are older than a grace period.
    refcount    integer NOT NULL DEFAULT 0 CHECK (refcount >= 0),

    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX blobs_storage_key_key ON blobs (storage_key);
-- Phase 2 turns this into whole-file dedup; for now it makes fsck's
-- "same content stored twice" report cheap.
CREATE INDEX blobs_sha256_idx ON blobs (sha256);
CREATE INDEX blobs_gc_idx ON blobs (created_at) WHERE refcount = 0;

-- ---------------------------------------------------------------------------
-- nodes — folders and files in one tree
-- ---------------------------------------------------------------------------
CREATE TABLE nodes (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- NULL parent means this is the owner's root. ON DELETE CASCADE gives
    -- subtree deletion for free when a node is purged.
    parent_id uuid REFERENCES nodes (id) ON DELETE CASCADE,

    kind      text NOT NULL CHECK (kind IN ('folder', 'file')),

    -- name is what the user sees; name_fold is what uniqueness is enforced on.
    -- Linux is case-sensitive, macOS and Windows are not, and WebDAV clients
    -- from those platforms will cheerfully try to create "Photos" beside
    -- "photos". Enforcing on the folded form makes the server behave the same
    -- way everywhere instead of producing pairs of files only some clients see.
    name      text NOT NULL CHECK (name <> '' AND name !~ '/'),
    name_fold text NOT NULL,

    -- Materialised absolute path ('/', '/photos', '/photos/2026/img.jpg').
    -- Denormalised on purpose: it makes subtree reads a prefix scan, which is
    -- what search in slice 7 and inherited ACLs in Phase 2 both want. The cost
    -- is rewriting descendants' paths on rename, which is bounded at this scale.
    path      text NOT NULL,

    -- Current version. FK added below, once file_versions exists.
    head_version_id uuid,

    -- Soft delete. Trashed nodes keep consuming quota until purged, which is
    -- the honest behaviour — the bytes really are still on disk.
    trashed_at timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- No two live siblings may share a folded name. Trashed nodes are excluded so
-- deleting "notes.txt" doesn't block creating a new one.
CREATE UNIQUE INDEX nodes_sibling_name_key
    ON nodes (parent_id, name_fold)
    WHERE trashed_at IS NULL;

-- Exactly one live root per owner.
CREATE UNIQUE INDEX nodes_owner_root_key
    ON nodes (owner_id)
    WHERE parent_id IS NULL AND trashed_at IS NULL;

CREATE INDEX nodes_owner_idx  ON nodes (owner_id);
CREATE INDEX nodes_parent_idx ON nodes (parent_id) WHERE trashed_at IS NULL;
CREATE INDEX nodes_trash_idx  ON nodes (owner_id, trashed_at) WHERE trashed_at IS NOT NULL;

-- Prefix scans over the materialised path. text_pattern_ops is required for
-- LIKE 'prefix%' to use the index on a non-C collation.
CREATE INDEX nodes_path_prefix_idx ON nodes (owner_id, path text_pattern_ops);

-- ---------------------------------------------------------------------------
-- file_versions — immutable snapshots of a file's content
-- ---------------------------------------------------------------------------
-- Slice 3 writes exactly one row per file and exposes no version history. The
-- table exists now so Phase 2 can add history without migrating live data.
CREATE TABLE file_versions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id    uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,

    -- RESTRICT, not CASCADE: deleting a blob out from under a live version
    -- would leave a file whose bytes are gone. GC must clear the reference
    -- first, and this constraint is what forces that ordering.
    blob_id    uuid NOT NULL REFERENCES blobs (id) ON DELETE RESTRICT,

    size       bigint NOT NULL CHECK (size >= 0),
    mime       text   NOT NULL DEFAULT 'application/octet-stream',

    created_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid REFERENCES users (id) ON DELETE SET NULL
);

CREATE INDEX file_versions_node_idx ON file_versions (node_id, created_at DESC);
CREATE INDEX file_versions_blob_idx ON file_versions (blob_id);

ALTER TABLE nodes
    ADD CONSTRAINT nodes_head_version_fk
    FOREIGN KEY (head_version_id) REFERENCES file_versions (id) ON DELETE SET NULL;

-- A file must have content and a folder must not. Catching this in the schema
-- means no application bug can produce a contentless file that the UI then has
-- to render defensively.
ALTER TABLE nodes
    ADD CONSTRAINT nodes_kind_head_check
    CHECK ((kind = 'folder' AND head_version_id IS NULL) OR kind = 'file');

-- +goose Down
ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_kind_head_check;
ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_head_version_fk;
DROP TABLE IF EXISTS file_versions;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS blobs;
