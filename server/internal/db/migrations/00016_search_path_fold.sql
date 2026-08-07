-- +goose Up
-- An index the path predicate can actually use.
--
-- Migration 00006 built nodes_path_trgm_idx over `path`, but Search matches with
-- `lower(n.path) LIKE $3` — the pattern is folded, so the column has to be folded
-- too. A trigram index on `path` cannot serve a predicate on `lower(path)`:
-- Postgres matches expression indexes by expression, and these two do not match.
-- The path arm of the search has therefore been a sequential scan since it was
-- written, silently, because the index it looks like it should use exists and is
-- simply never chosen.
--
-- (The content arm is fine. doc_text_trgm from migration 00012 does serve
-- `text ILIKE '%frag%'`; gin_trgm_ops supports unanchored patterns, which is the
-- whole reason it was chosen over full-text search.)
--
-- The fix is a functional index matching the query rather than a query rewritten
-- to match the index: folding is what makes the search case-insensitive, and
-- dropping it to reuse the old index would be a behaviour change to suit an
-- implementation detail.

CREATE INDEX nodes_path_fold_trgm_idx ON nodes USING gin (lower(path) gin_trgm_ops);

-- The old index is left in place: `path LIKE $1 || '/%'` — the subtree filter in
-- Search and the prefix rewrites in MoveOrRename and Trash — is anchored, unfolded
-- and genuinely uses it.

-- +goose Down
DROP INDEX IF EXISTS nodes_path_fold_trgm_idx;
