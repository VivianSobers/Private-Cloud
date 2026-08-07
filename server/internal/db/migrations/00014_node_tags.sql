-- +goose Up
-- Phase 4, slice 3b: tags on files.
--
-- Deliberately the cheap, explainable kind: MIME-derived categories and a small
-- keyword vocabulary over the extracted text, never a black-box classifier
-- guessing at photo contents. Every tag records its source, so a user can see
-- what the machine added and remove it — an auto-tagger that fights the user is
-- worse than none.
--
-- Per-node, NOT content-addressed like text and embeddings: a tag is about the
-- file (its name, where it lives, a decision the user made about it), and user
-- tags are inherently per-file. Auto tags are re-derived on each extraction and
-- replace only the previous AUTO tags, so a user's tags are never clobbered.

CREATE TABLE node_tags (
    node_id uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    tag     text NOT NULL,

    -- 'auto' (machine-derived, replaceable) or 'user' (explicit, never touched by
    -- re-tagging). A tag a user added stays a user tag even if the auto-tagger
    -- would also have produced it.
    source text NOT NULL DEFAULT 'auto' CHECK (source IN ('auto', 'user')),

    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (node_id, tag)
);

-- Listing files by tag, and counting tags for a user, both scan by tag.
CREATE INDEX node_tags_tag ON node_tags (tag);

-- +goose Down
DROP TABLE IF EXISTS node_tags;
