-- +goose Up
-- An index for the queue-depth poll.
--
-- The API publishes job counts as gauges every 30 seconds with
-- `SELECT state, count(*) FROM jobs GROUP BY state`. Nothing indexes state on its
-- own -- jobs_claim_kind and jobs_pending_unique are both partial to
-- state = 'queued', so neither can answer a query that needs all four buckets --
-- so the poll is a full sequential scan of the table, twice a minute, forever, on
-- a machine with one spinning disk.
--
-- The table is meant to stay small, but "meant to" is doing a lot of work there:
-- done and failed rows live for PC_JOB_RETENTION (7 days by default) and the
-- pruner only runs if a worker is up to run it. A stopped worker is exactly when
-- the queue is deepest and the poll is most expensive.
--
-- A plain btree on state lets the count come from an index-only scan instead.

CREATE INDEX jobs_state ON jobs (state);

-- +goose Down
DROP INDEX IF EXISTS jobs_state;
