-- +goose Up
-- Phase 7: user-to-user grants and the audit log.
--
-- The compatibility hazard this phase carries is recorded in the API contract:
-- every query in the system filters owner_id = $me, and introducing "files I can
-- see but do not own" widens what the existing endpoints mean. Nothing in this
-- migration changes that by itself — the widening is opt-in per request
-- (?include_shared=true) and lives in the query layer. What the schema has to
-- get right is that a grant NEVER moves or copies anything: the file stays in
-- the owner's tree, and access is a separate fact about it.

CREATE TABLE grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The node the grant is ON. Inheritance is derived from the node's
    -- materialised path rather than stored per descendant: a folder share must
    -- cover files that do not exist yet, and expanding a grant into one row per
    -- descendant would have to be maintained on every create, move and rename.
    node_id uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,

    -- Who granted it. Denormalised from nodes.owner_id so revoking survives the
    -- owner losing the node, and so "what have I shared out" is one index scan.
    owner_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    grantee_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Three roles, and only two are grantable. 'owner' is the file's actual
    -- owner and cannot be handed over — a CHECK rather than a comment, because
    -- an 'owner' row here would be a second, contradictory source of truth about
    -- who owns a node.
    role text NOT NULL CHECK (role IN ('viewer', 'editor')),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Re-granting the same node to the same person is a role change, not a
    -- second grant. Without this, revoking would have to delete an unknown
    -- number of rows and a UI would show duplicates.
    UNIQUE (node_id, grantee_id),

    -- Granting to yourself is meaningless and would make "shared with me"
    -- include your own tree.
    CHECK (owner_id <> grantee_id)
);

-- "What can this person reach?" — the hot path for every ACL check.
CREATE INDEX grants_grantee ON grants (grantee_id);
-- "What have I shared out?" — the owner's side of GET /grants.
CREATE INDEX grants_owner ON grants (owner_id);

-- ---------------------------------------------------------------------------
-- audit_log — authorisation-relevant events only
-- ---------------------------------------------------------------------------
-- Grants, role changes, logins, admin actions, share creation. NOT every read:
-- a log that records everything is one nobody reads, and on this hardware it
-- would outgrow the files it describes.
CREATE TABLE audit_log (
    id bigserial PRIMARY KEY,

    at timestamptz NOT NULL DEFAULT now(),

    -- SET NULL rather than CASCADE: deleting a user must not erase the record of
    -- what they did. That is the one property an audit log has to have.
    actor_id   uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_name text NOT NULL DEFAULT '',

    action text NOT NULL,
    target text NOT NULL DEFAULT '',

    -- Ties an entry back to the API access log without a second correlation
    -- scheme.
    request_id text NOT NULL DEFAULT '',

    -- Free-form context. jsonb rather than text so a later query can filter on
    -- it without reparsing every row.
    detail jsonb NOT NULL DEFAULT '{}'::jsonb
);

-- The admin console reads newest-first, optionally filtered by actor or action.
CREATE INDEX audit_log_at ON audit_log (at DESC);
CREATE INDEX audit_log_actor ON audit_log (actor_id, at DESC);
CREATE INDEX audit_log_action ON audit_log (action, at DESC);

-- +goose Down
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS grants;
