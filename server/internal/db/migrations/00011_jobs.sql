-- +goose Up
-- Phase 4, slice 1: a durable, DB-backed work queue.
--
-- The substrate every later intelligence feature rides on. OCR and embeddings
-- must run OUT of band — never inline with an upload, never inside the API
-- process — because the always-on box is 7 GiB and one spinner and cannot afford
-- a resident model. So work is enqueued as a row here and drained by a separate
-- `pcworker` process. Postgres is already the source of truth, so the queue is a
-- table, not a broker: one fewer moving part, and the claim is transactional.

CREATE TABLE jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- What to do, and to what. node_id is nullable for owner-scoped work that is
    -- not about one node; both foreign keys CASCADE, so purging a file or deleting
    -- an account takes its pending work with it rather than leaving jobs that
    -- reference rows no longer there.
    kind     text NOT NULL,
    node_id  uuid REFERENCES nodes (id) ON DELETE CASCADE,
    owner_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    state text NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'running', 'done', 'failed')),

    -- Retry accounting. A job that exhausts max_attempts lands in 'failed' with
    -- its last_error, rather than looping forever and pinning a core this machine
    -- cannot spare. run_after pushes a retry into the future for backoff, and is
    -- also the scheduled-start time for work deliberately deferred.
    attempts     int NOT NULL DEFAULT 0,
    max_attempts int NOT NULL DEFAULT 5,
    run_after    timestamptz NOT NULL DEFAULT now(),

    -- When the current 'running' claim was taken. A worker that crashes mid-job
    -- leaves the row 'running'; the reaper uses this to return a job whose lease
    -- has expired to 'queued', so a crash costs a retry, not a lost job.
    claimed_at timestamptz,

    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The claim query scans only queued, runnable jobs, oldest first. A partial index
-- keeps it off the done/failed rows that accumulate until pruned.
CREATE INDEX jobs_claim ON jobs (run_after) WHERE state = 'queued';

-- At most one pending or in-flight job per (kind, node_id): enqueuing extraction
-- for a file that already has extraction pending is a no-op, so a burst of edits
-- to one file does not queue a burst of duplicate work. NULL node_ids are treated
-- as distinct by the unique index, so owner-scoped jobs are never collapsed.
CREATE UNIQUE INDEX jobs_pending_unique ON jobs (kind, node_id)
    WHERE state IN ('queued', 'running');

CREATE INDEX jobs_owner ON jobs (owner_id);

-- +goose Down
DROP TABLE IF EXISTS jobs;
