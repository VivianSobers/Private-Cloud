-- +goose Up
-- Phase 1, slice 1.
--
-- The domain schema (users, nodes, file_versions, blobs) lands in slice 2
-- alongside auth, where its shape can be reviewed on its own merits. This
-- first migration only installs the extensions later migrations depend on.
--
-- Extensions are separated deliberately: CREATE EXTENSION needs elevated
-- privileges, and isolating it means a permissions failure surfaces here with
-- an obvious cause rather than midway through a table migration.

-- pg_trgm powers filename/path search in slice 7 (trigram indexes make
-- ILIKE '%foo%' fast, which a btree index cannot do).
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- +goose Down
-- Dropping pg_trgm would break any index built on it. Down migrations exist to
-- undo a bad deploy, and this one has no safe automatic inverse, so it is a
-- no-op by design. Removing the extension is a deliberate manual act.
SELECT 1;
