-- +goose Up
-- Phase 3, slice 1: the change journal.
--
-- A sync client that has been offline asks one question — "what changed since I
-- last looked?" — and the journal answers it with a cursor. Every sync-relevant
-- change to a node appends a row; the client resumes from the last seq it saw.

-- ---------------------------------------------------------------------------
-- sync_state — the per-owner change counter
-- ---------------------------------------------------------------------------
-- The cursor is a PER-OWNER counter, not a global bigserial, and that choice is
-- load-bearing. A bigserial assigns numbers at INSERT time, so a transaction
-- holding seq 9 can commit AFTER one holding seq 10 — and a client that advanced
-- its cursor to 10 would then never see 9 when it finally commits. Bumping a
-- counter inside the writing transaction serialises assignment behind this row's
-- lock, so seq order equals COMMIT order: a client that sees seq N is guaranteed
-- every lower seq is already visible. The price is that one owner's concurrent
-- writes serialise on their own counter — negligible for a personal cloud, and
-- the right trade for a cursor that cannot skip a change.
CREATE TABLE sync_state (
    owner_id   uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    change_seq bigint NOT NULL DEFAULT 0
);

-- ---------------------------------------------------------------------------
-- changes — the append-only journal
-- ---------------------------------------------------------------------------
CREATE TABLE changes (
    owner_id uuid   NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    seq      bigint NOT NULL,

    -- The node that changed. NOT a foreign key: a 'delete' records a node that no
    -- longer exists, and the change matters precisely because the row is gone.
    node_id  uuid   NOT NULL,

    -- 'upsert' means the node is live at its current state; 'delete' means it has
    -- left the live tree (trashed or purged). The row is an INVALIDATION, not a
    -- snapshot — the client re-fetches current state, so a change immediately
    -- superseded by a later one is self-healing rather than stale.
    kind     text   NOT NULL CHECK (kind IN ('upsert', 'delete')),

    at       timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (owner_id, seq)
);

-- The one query every client makes: "my changes past cursor N, in order".
CREATE INDEX changes_owner_seq_idx ON changes (owner_id, seq);
-- Retention prunes the tail by age.
CREATE INDEX changes_at_idx ON changes (at);

-- ---------------------------------------------------------------------------
-- journal trigger
-- ---------------------------------------------------------------------------
-- A trigger, for the same reason refcounts are: a move rewrites every
-- descendant's path in one statement, and a purge cascades rows no Go code ever
-- names. Service-layer journaling would have to reimplement both and would drift
-- the first time a new write path appeared. The trigger fires on every node write
-- but records one only when something a client cares about — path, head version,
-- or trashed state — actually changed.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION journal_node_change() RETURNS trigger AS $$
DECLARE
    owner uuid;
    node  uuid;
    op    text;
    s     bigint;
BEGIN
    IF TG_OP = 'DELETE' THEN
        owner := OLD.owner_id;
        node  := OLD.id;
        op    := 'delete';
    ELSE
        owner := NEW.owner_id;
        node  := NEW.id;
        IF NEW.trashed_at IS NULL THEN
            op := 'upsert';
        ELSE
            op := 'delete';
        END IF;

        -- An update that touched nothing sync-relevant (an updated_at bump, say)
        -- is not a change worth a cursor advance.
        IF TG_OP = 'UPDATE'
           AND NEW.path IS NOT DISTINCT FROM OLD.path
           AND NEW.head_version_id IS NOT DISTINCT FROM OLD.head_version_id
           AND NEW.trashed_at IS NOT DISTINCT FROM OLD.trashed_at THEN
            RETURN NEW;
        END IF;
    END IF;

    -- Bump the owner's counter and take its lock in one statement: the lock is
    -- held to commit, so seq is assigned in commit order.
    INSERT INTO sync_state (owner_id, change_seq) VALUES (owner, 1)
        ON CONFLICT (owner_id) DO UPDATE SET change_seq = sync_state.change_seq + 1
        RETURNING change_seq INTO s;

    INSERT INTO changes (owner_id, seq, node_id, kind) VALUES (owner, s, node, op);

    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER nodes_journal
    AFTER INSERT OR UPDATE OR DELETE ON nodes
    FOR EACH ROW EXECUTE FUNCTION journal_node_change();

-- +goose Down
DROP TRIGGER IF EXISTS nodes_journal ON nodes;
DROP FUNCTION IF EXISTS journal_node_change();
DROP TABLE IF EXISTS changes;
DROP TABLE IF EXISTS sync_state;
