-- +goose Up
-- Make the claim index match the claim query.
--
-- jobs_claim from migration 00011 is (run_after) WHERE state = 'queued', but the
-- claim also filters on kind:
--
--   WHERE state = 'queued' AND run_after <= now() AND kind = ANY($1)
--
-- Without kind in the index, a worker scans queued jobs in run_after order and
-- discards every one whose kind it does not handle. That is free on a queue where
-- one worker handles everything, and quadratic-feeling on the split-tier setup
-- the design actually recommends: an always-on box that only handles `extract`
-- walks past the whole embed backlog on every single claim.
--
-- kind leads because it is the equality predicate; run_after follows so the
-- ordering the claim asks for still comes from the index rather than a sort.

CREATE INDEX jobs_claim_kind ON jobs (kind, run_after) WHERE state = 'queued';
DROP INDEX IF EXISTS jobs_claim;

-- +goose Down
CREATE INDEX jobs_claim ON jobs (run_after) WHERE state = 'queued';
DROP INDEX IF EXISTS jobs_claim_kind;
