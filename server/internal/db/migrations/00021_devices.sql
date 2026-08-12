-- +goose Up
-- Phase 6: devices and push subscriptions.
--
-- A device IS a session of kind 'device' — the row minted by POST /auth/token.
-- Nothing new is created here to represent one, deliberately: a second table
-- keyed to the session would have to be kept in step with revocation, and the
-- whole point of "I lost my laptop" working is that revoking the session IS
-- revoking the device, with no second place for the two to disagree.
--
-- What a session lacks is a human name. `user_agent` is what the client called
-- itself, which is frequently "Go-http-client/2.0"; a person needs to be able to
-- call it "the laptop".

ALTER TABLE sessions ADD COLUMN device_name text NOT NULL DEFAULT '';

-- Web Push subscriptions.
--
-- Deliberately a HOOK, not a service: this server does not talk to APNs or FCM
-- and should not learn to. It stores what a client registered so that something
-- else can deliver, and a client that registers nothing simply polls
-- GET /changes — the existing, working path. Push is a latency optimisation and
-- never a correctness requirement.
CREATE TABLE push_subscriptions (
    -- One subscription per device. ON DELETE CASCADE so revoking a session takes
    -- its push registration with it: a revoked device must stop being a delivery
    -- target in the same instant it stops being able to read anything.
    session_id uuid PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- The push service's URL for this subscriber, plus the two keys the Web Push
    -- encryption scheme needs. Opaque to us; we never parse them.
    endpoint text NOT NULL CHECK (length(endpoint) > 0),
    p256dh   text NOT NULL,
    auth_key text NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- "Which subscriptions should this user's change notify?" is the only read.
CREATE INDEX push_subscriptions_user ON push_subscriptions (user_id);

-- +goose Down
DROP TABLE IF EXISTS push_subscriptions;
ALTER TABLE sessions DROP COLUMN IF EXISTS device_name;
