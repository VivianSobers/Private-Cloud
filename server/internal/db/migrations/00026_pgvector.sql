-- +goose Up
-- Phase 4 / Phase 8: the pgvector upgrade path, taken without making pgvector
-- a requirement.
--
-- The exact cosine scan in Go is correct and stays the fallback. What it is not
-- is complete at scale: SemanticSearch bounds its work with maxSemanticScan and
-- an `ORDER BY updated_at DESC` truncation, so past that many candidate rows a
-- query silently ranks the most RECENT vectors rather than the most SIMILAR
-- ones. That is the defect this closes — the index is the speed, but ordering
-- by distance in SQL is the correctness.
--
-- Why the extension is optional rather than required. The nightly pg_dumpall is
-- restorable onto a bare machine, and runbook-restore.md leans on that: it is
-- the path out of a total loss. A migration that hard-requires an extension
-- makes every future restore depend on that extension being present on whatever
-- machine you are rebuilding onto, at the worst possible moment. So this
-- migration succeeds on stock Postgres and simply leaves the column absent.
--
-- `vec` is declared without a dimension on purpose. doc_embedding holds several
-- models side by side by design, and their widths differ; a vector(N) column
-- would force one model per deployment forever and break the "old vectors live
-- alongside new ones" property that makes a model change safe. The per-width
-- HNSW indexes are partial + expression indexes instead, created by
-- `cloudctl embeddings index`, because the width is a property of the deployed
-- model rather than of the schema.
--
-- bytea stays the source of truth. `vec` is a derived copy: it is written
-- alongside on every write, and `cloudctl embeddings backfill` fills it for rows
-- written before this migration. Anything that cannot read `vec` still gets a
-- correct answer from the packed bytes, so a half-backfilled table is slow, not
-- wrong.
-- +goose StatementBegin
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS vector;
EXCEPTION WHEN OTHERS THEN
    -- insufficient_privilege or undefined_file: no pgvector on this server.
    RAISE NOTICE 'pgvector unavailable (%); semantic search keeps the exact scan', SQLERRM;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
        ALTER TABLE doc_embedding ADD COLUMN IF NOT EXISTS vec vector;

        -- The pending set: rows whose `vec` has not been filled in yet.
        --
        -- This exists so the read path can ask "is this model's copy complete?"
        -- for the price of an index probe rather than a table scan. A partial
        -- index over `vec IS NULL` holds one entry per un-backfilled row and
        -- NOTHING at all once the backfill finishes, which is the steady state.
        --
        -- It is what keeps the indexed path honest: ordering by distance in SQL
        -- would silently skip a row whose vec is still NULL, so ranking that way
        -- before the copy is complete would drop documents from results rather
        -- than merely being slower. The read path checks this first and stays on
        -- the exact scan until the answer is no.
        CREATE INDEX IF NOT EXISTS doc_embedding_vec_pending
            ON doc_embedding (model, dim) WHERE vec IS NULL;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- The extension is deliberately NOT dropped: other databases or tables in the
-- same cluster may be using it, and DROP EXTENSION would cascade to them.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
        ALTER TABLE doc_embedding DROP COLUMN IF EXISTS vec;
    END IF;
END
$$;
-- +goose StatementEnd
