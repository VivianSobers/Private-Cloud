-- +goose Up
-- Phase 1, slice 7: search.
--
-- Trigram indexes on name and path, not full-text search. The distinction
-- matters and is deliberate:
--
--   * to_tsvector stems words and matches on token boundaries. It is the right
--     tool for searching DOCUMENT CONTENT, which arrives in Phase 4 alongside
--     OCR and embeddings.
--   * Filenames are not prose. People search them by fragment — "budg" should
--     find "budget-2026-final.xlsx", and "2026" should find it too. Full-text
--     search finds neither, because neither is a token in that filename.
--
-- pg_trgm was enabled in migration 00001 for exactly this.

-- gin_trgm_ops supports LIKE '%frag%' and the similarity operators. A btree
-- index cannot help an unanchored pattern at all — without these, every search
-- is a sequential scan of the whole tree.
CREATE INDEX nodes_name_trgm_idx ON nodes USING gin (name_fold gin_trgm_ops);

-- Path as well as name, so "photos/2026" matches without the caller having to
-- know whether the fragment spans a directory boundary.
CREATE INDEX nodes_path_trgm_idx ON nodes USING gin (path gin_trgm_ops);

-- +goose Down
DROP INDEX IF EXISTS nodes_path_trgm_idx;
DROP INDEX IF EXISTS nodes_name_trgm_idx;
