-- +goose Up
-- Phase 5, slice 2: albums.
--
-- An album is NOT a folder, and the distinction is the whole design:
--
--   * A node lives in exactly one folder. It can be in many albums.
--   * Adding to an album does not move the file, and removing from one does not
--     delete it. The tree stays whatever the user or the sync client made it.
--   * Albums are ordered by hand. Folders are ordered by name.
--
-- Modelling an album as a folder would mean either moving files (breaking the
-- sync client's view of the tree) or copying them (breaking dedup and quota).
-- A join table is the honest shape.

CREATE TABLE albums (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    name        text NOT NULL CHECK (length(btrim(name)) > 0),
    description text NOT NULL DEFAULT '',

    -- The picture shown on the album tile. SET NULL rather than CASCADE: losing
    -- the cover photo must not delete the album, it must fall back to picking
    -- another item.
    cover_node_id uuid REFERENCES nodes (id) ON DELETE SET NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX albums_owner ON albums (owner_id, updated_at DESC);

CREATE TABLE album_items (
    album_id uuid NOT NULL REFERENCES albums (id) ON DELETE CASCADE,

    -- CASCADE, so purging a file removes it from every album it was in. A
    -- dangling item would render as a broken tile with no way to clear it.
    node_id uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,

    -- Explicit user ordering. Not a float or a gap-based scheme: reordering
    -- rewrites the whole album in one statement (see PATCH /albums/{id}/items),
    -- which is cheap at the scale one person's album reaches and avoids the
    -- rebalancing that fractional ranks eventually need.
    position int NOT NULL DEFAULT 0,

    added_at timestamptz NOT NULL DEFAULT now(),

    -- The primary key is what makes adding a node twice a no-op rather than a
    -- duplicate tile, so a retried request is safe.
    PRIMARY KEY (album_id, node_id)
);

-- Reading an album is "its items, in order".
CREATE INDEX album_items_order ON album_items (album_id, position, added_at);

-- And the reverse: "which albums is this file in?", for a file's detail view and
-- for cleaning up when a node is purged.
CREATE INDEX album_items_node ON album_items (node_id);

-- +goose Down
DROP TABLE IF EXISTS album_items;
DROP TABLE IF EXISTS albums;
