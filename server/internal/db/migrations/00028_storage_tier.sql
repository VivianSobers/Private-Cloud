-- +goose Up
-- Phase 9, slice 3: which tier holds a blob's or a chunk's bytes.
--
-- The cold tier is a LOCATION, never a visibility change. A demoted file is
-- still listed, still searchable, still has its metadata and its versions; only
-- where the bytes live moves. So this migration adds no new table and changes no
-- relationship — it annotates the two rows that already name a storage key.
--
-- Both tables get the same three columns rather than one shared table, because
-- blobs and chunks are already accounted, garbage-collected and fsck'd
-- separately and joining them here would give every one of those code paths a
-- new way to disagree.
--
-- 'hot' is the default and every existing row gets it, which is the truth: with
-- no cold tier configured, everything is on the local pool.

-- ---------------------------------------------------------------------------
-- blobs
-- ---------------------------------------------------------------------------
ALTER TABLE blobs
    -- 'hot'  — the bytes are on the local pool, and may ALSO be in the bucket
    --          (a promotion leaves the cold copy in place; see TieredStore.promote).
    -- 'cold' — the bytes are in object storage and NOT on the local pool.
    --
    -- There is deliberately no 'restoring' state in the database. A restore is
    -- an in-flight operation of one process, not a fact about the content, and
    -- a row that says 'restoring' after the worker holding it was killed is a
    -- lie no other process can safely clear. The API answers "restoring"
    -- because the store it asked is restoring right now.
    ADD COLUMN tier text NOT NULL DEFAULT 'hot'
        CHECK (tier IN ('hot', 'cold')),

    -- When the tier last changed, so an operator can see how much has moved
    -- recently and a bad policy can be recognised before it has drained the pool.
    ADD COLUMN tiered_at timestamptz,

    -- Last time these bytes were READ. Nullable, and null means "never read
    -- since this column existed" — deliberately NOT backfilled to now(), which
    -- would make every pre-existing blob look freshly touched and postpone the
    -- first demotion by a whole idle period. The policy falls back to
    -- created_at, which is the honest lower bound on when it was last wanted.
    ADD COLUMN last_access_at timestamptz;

-- The demotion scan: hot rows, oldest access first. Partial to 'hot' because
-- the scan never looks at cold rows, and a full index would keep the whole
-- table's worth of entries for the sake of the half it never reads.
CREATE INDEX blobs_tier_scan_idx ON blobs (coalesce(last_access_at, created_at))
    WHERE tier = 'hot';

-- The accounting query behind GET /admin/storage, and fsck's "which rows should
-- I expect to be absent from local disk".
CREATE INDEX blobs_cold_idx ON blobs (tier) WHERE tier = 'cold';

-- ---------------------------------------------------------------------------
-- chunks
-- ---------------------------------------------------------------------------
-- The same three columns, and the stakes are higher here for the same reason
-- every chunk comment in this schema says: a chunk is SHARED. Demoting one
-- moves bytes out from under every file that references it, possibly belonging
-- to people who were never involved — which is why the read path checks a
-- manifest's chunk tiers in one query before it starts writing a response, and
-- why the policy job's access recency is per chunk rather than per file.
ALTER TABLE chunks
    ADD COLUMN tier text NOT NULL DEFAULT 'hot'
        CHECK (tier IN ('hot', 'cold')),
    ADD COLUMN tiered_at timestamptz,
    ADD COLUMN last_access_at timestamptz;

CREATE INDEX chunks_tier_scan_idx ON chunks (coalesce(last_access_at, created_at))
    WHERE tier = 'hot';
CREATE INDEX chunks_cold_idx ON chunks (tier) WHERE tier = 'cold';

-- +goose Down
-- The refusal comes FIRST, before anything is dropped, exactly as migration
-- 00007's does. Rolling this back on a deployment that has demoted content
-- destroys the only record of which bytes are NOT on the local pool — and fsck,
-- reading a schema with no tier column, then sees every cold blob as content
-- with a row and no bytes. That is the report that sends an operator to a
-- backup for files that are perfectly safe, and it is one flag away from
-- `--repair` on a future schema treating them as deletable.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM blobs WHERE tier <> 'hot')
       OR EXISTS (SELECT 1 FROM chunks WHERE tier <> 'hot') THEN
        RAISE EXCEPTION 'cannot roll back: % blob(s) and % chunk(s) are in the cold tier. Promote them first (cloudctl tier restore --all).',
            (SELECT count(*) FROM blobs WHERE tier <> 'hot'),
            (SELECT count(*) FROM chunks WHERE tier <> 'hot');
    END IF;
END $$;
-- +goose StatementEnd

DROP INDEX IF EXISTS chunks_cold_idx;
DROP INDEX IF EXISTS chunks_tier_scan_idx;
ALTER TABLE chunks
    DROP COLUMN IF EXISTS last_access_at,
    DROP COLUMN IF EXISTS tiered_at,
    DROP COLUMN IF EXISTS tier;

DROP INDEX IF EXISTS blobs_cold_idx;
DROP INDEX IF EXISTS blobs_tier_scan_idx;
ALTER TABLE blobs
    DROP COLUMN IF EXISTS last_access_at,
    DROP COLUMN IF EXISTS tiered_at,
    DROP COLUMN IF EXISTS tier;
