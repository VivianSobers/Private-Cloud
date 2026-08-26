-- +goose Up
-- Phase 9 slice 5: billing HOOKS — the integration seam, not a billing system.
--
-- The honest position first, because it is what this schema is shaped by. There
-- is no second tenant, no payment provider has been chosen, and the thing
-- billing would attach to is one person's disk. What is missing is therefore not
-- an invoice generator; it is the seam an invoice generator would need, and the
-- record that makes a past billing period answerable at all. A period that was
-- never measured cannot be re-measured later — the bytes have moved on — so the
-- snapshot has to be taken while the period is running, whatever is eventually
-- done with it.
--
-- The rule this migration exists to obey: THERE IS EXACTLY ONE NOTION OF "USED".
-- Quota enforcement (checkQuota) and GET /usage already answer from `usageBytes`
-- in internal/files/store.go, down to the query text, precisely because two
-- notions of a number disagree eventually and then nobody knows which to believe
-- at the moment it matters. So metering_records stores a SNAPSHOT of that same
-- answer, and adds no accounting of its own. Nothing in this file recomputes
-- bytes.

-- A plan is a named quota, optionally with price metadata attached.
--
-- Price is metadata and nothing more: no currency conversion, no proration, no
-- tax, no invoice. Recording an amount beside a plan is what lets a future
-- provider integration be a mapping rather than a schema change, and pretending
-- to more than that would be inventing a business this repository does not have.
CREATE TABLE billing_plans (
    id   uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The stable handle an operator and any future provider both name. Folded to
    -- lower case by the application before it gets here, so "Free" and "free"
    -- cannot become two plans that look identical in a list.
    name text NOT NULL UNIQUE CHECK (name <> '' AND length(name) <= 64),

    description text NOT NULL DEFAULT '',

    -- NULL means unlimited, exactly as users.quota_bytes does. Zero would be a
    -- plan of zero bytes, which is the opposite instruction — the same
    -- absent-versus-null distinction Phase 7 had to get right on the user row,
    -- and it is deliberately spelled the same way here so nobody has to hold two
    -- conventions in their head.
    quota_bytes bigint CHECK (quota_bytes IS NULL OR quota_bytes >= 0),

    -- Optional price metadata. Integer minor units (cents), never a float:
    -- money in a float is a rounding error waiting for a customer to find it.
    -- Currency is ISO-4217 and only meaningful alongside an amount, which the
    -- CHECK enforces rather than leaving to a comment.
    price_cents  integer CHECK (price_cents IS NULL OR price_cents >= 0),
    currency     text    CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
    period       text    NOT NULL DEFAULT 'monthly'
                         CHECK (period IN ('monthly', 'yearly')),
    CHECK (price_cents IS NULL OR currency IS NOT NULL),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One plan per account, and the account row is the primary key: an account on
-- two plans has no answerable quota, so the schema refuses it rather than
-- leaving the application to pick a winner.
--
-- ON DELETE CASCADE on the user, because a plan assignment describes an account
-- and means nothing without one. ON DELETE RESTRICT on the plan, because
-- deleting a plan out from under live accounts would silently strand them: the
-- operator has to move the accounts first, which is the conversation that
-- deletion actually requires.
CREATE TABLE account_plans (
    user_id uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    plan_id uuid NOT NULL     REFERENCES billing_plans (id) ON DELETE RESTRICT,

    assigned_at timestamptz NOT NULL DEFAULT now(),

    -- Who assigned it. SET NULL rather than CASCADE for the same reason the
    -- audit log denormalises actor_name: losing the administrator must not
    -- destroy the record of the decision.
    assigned_by uuid REFERENCES users (id) ON DELETE SET NULL
);

CREATE INDEX account_plans_plan ON account_plans (plan_id);

-- A periodic snapshot of one owner's usage, so a billing period can be answered
-- after it has closed.
--
-- Every byte column here is COPIED from files.Usage — the numbers quota
-- enforcement and GET /usage already report — and is never recomputed. The four
-- are stored separately rather than as one total for the reason the admin user
-- list stores them separately: a full account needs to be explicable as "empty
-- the trash", "wait for retention" or "buy a disk", and a single total explains
-- none of those.
CREATE TABLE metering_records (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    owner_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- The half-open period [period_start, period_end). Computed by the metering
    -- task from a calendar boundary in UTC, never from "now minus a month" — an
    -- interval-arithmetic period drifts, and two runs a millisecond apart would
    -- land in different periods.
    period_start timestamptz NOT NULL,
    period_end   timestamptz NOT NULL,
    CHECK (period_end > period_start),

    -- The snapshot, from files.Usage.
    live_bytes    bigint NOT NULL CHECK (live_bytes    >= 0),
    trash_bytes   bigint NOT NULL CHECK (trash_bytes   >= 0),
    version_bytes bigint NOT NULL CHECK (version_bytes >= 0),
    file_count    bigint NOT NULL CHECK (file_count    >= 0),

    -- Both the last observation and the peak, because which one a bill should be
    -- based on is a business decision nobody here is entitled to make. Recording
    -- both means the decision can be taken later against real data instead of
    -- being silently baked in now by whichever number happened to be stored.
    peak_total_bytes bigint NOT NULL CHECK (peak_total_bytes >= 0),

    -- How many times the task has written into this row. It is the difference
    -- between "this account used nothing" and "nobody was measuring", which is
    -- the single most useful fact when a period looks wrong months later.
    samples integer NOT NULL DEFAULT 1 CHECK (samples > 0),

    -- What the account was ON at the time, denormalised. A plan renamed or
    -- repriced afterwards must not rewrite history: a closed period has to keep
    -- saying what it actually said.
    plan_id     uuid REFERENCES billing_plans (id) ON DELETE SET NULL,
    plan_name   text NOT NULL DEFAULT '',
    quota_bytes bigint CHECK (quota_bytes IS NULL OR quota_bytes >= 0),

    first_seen_at timestamptz NOT NULL DEFAULT now(),
    recorded_at   timestamptz NOT NULL DEFAULT now(),

    -- One row per owner per period, upserted. This is what makes the metering
    -- task idempotent and therefore safe to run on any cadence, restart or
    -- retry: running it twice in a minute updates one row instead of inventing
    -- a second, contradictory measurement of the same period.
    UNIQUE (owner_id, period_start)
);

-- "What did this account do over these months" — the read the admin endpoint
-- and any future invoice both perform.
CREATE INDEX metering_records_owner_period
    ON metering_records (owner_id, period_start DESC);
-- "Show me the period everybody was in" — the cross-account read, which has no
-- owner to narrow it.
CREATE INDEX metering_records_period ON metering_records (period_start DESC);

-- +goose Down
-- Dropped in dependency order. account_plans references both users and
-- billing_plans; metering_records references billing_plans with SET NULL, which
-- still holds a dependency, so the plans table goes last.
DROP TABLE IF EXISTS metering_records;
DROP TABLE IF EXISTS account_plans;
DROP TABLE IF EXISTS billing_plans;
