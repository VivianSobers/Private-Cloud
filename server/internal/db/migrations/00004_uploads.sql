-- +goose Up
-- Phase 1, slice 4: resumable uploads (tus 1.0.0).
--
-- The simple POST /upload endpoint from slice 3 is fine for small files, but a
-- 4 GB video over a phone connection will fail eventually, and starting over
-- from zero is the difference between a usable file server and a toy. tus is
-- the protocol here because clients already exist for it (uppy in the browser,
-- tus-js-client, tus-py-client) — inventing a bespoke chunk protocol would mean
-- writing every one of those clients too.

CREATE TABLE upload_sessions (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Where the finished file will land. Resolved at creation so an upload
    -- into a folder that is later deleted fails at creation-time semantics
    -- rather than after the user has waited for 4 GB to transfer.
    parent_id uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    name      text NOT NULL,
    mime      text NOT NULL DEFAULT 'application/octet-stream',

    -- Total size the client declared via Upload-Length. Upload-Defer-Length is
    -- deliberately unsupported: without a declared size there is no way to
    -- check quota before accepting the bytes.
    size      bigint NOT NULL CHECK (size >= 0),

    -- Bytes durably written and accounted for. This is the ONLY authority on
    -- progress; the staging file may briefly hold more after a crash mid-PATCH,
    -- and is truncated back to this value before the next append.
    upload_offset bigint NOT NULL DEFAULT 0 CHECK (upload_offset >= 0),

    -- Path of the partial file within the blob store's staging area.
    staging_key text NOT NULL,

    -- Serialised SHA-256 state, so the hash is computed incrementally across
    -- requests instead of re-reading the whole file at the end. crypto/sha256
    -- implements BinaryMarshaler precisely for this. Committed in the same
    -- statement as upload_offset, which is what keeps the two consistent.
    hash_state bytea,

    -- Cooperative lock. tus requires the server to reject concurrent PATCHes
    -- to one upload; two interleaved writers would produce a file that is the
    -- right length and the wrong content. A timestamp rather than a boolean so
    -- a client that dies mid-chunk does not wedge its own upload forever.
    locked_until timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Abandoned uploads must not occupy disk indefinitely. Surfaced to clients
    -- through the tus expiration extension.
    expires_at timestamptz NOT NULL,

    CONSTRAINT upload_sessions_offset_check CHECK (upload_offset <= size)
);

CREATE INDEX upload_sessions_owner_idx   ON upload_sessions (owner_id, created_at DESC);
CREATE INDEX upload_sessions_expiry_idx  ON upload_sessions (expires_at);

-- +goose Down
DROP TABLE IF EXISTS upload_sessions;
