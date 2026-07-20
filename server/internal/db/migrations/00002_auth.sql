-- +goose Up
-- Phase 1, slice 2: identity and authentication.
--
-- Multi-user tables from the start, single-user UI. Single-user assumptions
-- spread into every query and every handler; the schema cost of doing it
-- properly now is close to zero, and unwinding it later is not.

-- ---------------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- username is what the user typed (display form); username_fold is the
    -- case-folded form that uniqueness is actually enforced on. Without this,
    -- "Guru" and "guru" are two accounts, and every client that lowercases
    -- input finds the wrong one.
    username      text NOT NULL,
    username_fold text NOT NULL,
    display_name  text NOT NULL DEFAULT '',

    is_admin      boolean NOT NULL DEFAULT false,

    -- NULL means "no quota". Enforcement arrives with file storage in slice 3;
    -- the column exists now so adding it later isn't a migration over live data.
    quota_bytes   bigint,

    -- Soft lock. Deleting a user would cascade away their files, which is
    -- almost never what "disable this account" should mean.
    disabled_at   timestamptz,

    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_username_fold_key ON users (username_fold);

-- ---------------------------------------------------------------------------
-- webauthn_credentials — one row per passkey
-- ---------------------------------------------------------------------------
-- A user may register several (laptop, phone, hardware key). That is the
-- primary defence against lockout, alongside recovery codes.
CREATE TABLE webauthn_credentials (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    credential_id    bytea NOT NULL,
    public_key       bytea NOT NULL,
    attestation_type text  NOT NULL DEFAULT '',
    aaguid           bytea NOT NULL DEFAULT ''::bytea,

    -- Incremented by the authenticator on each use. A sign_count that goes
    -- backwards suggests a cloned authenticator; go-webauthn reports it and we
    -- persist the flag rather than silently discarding the signal.
    sign_count       bigint  NOT NULL DEFAULT 0,
    clone_warning    boolean NOT NULL DEFAULT false,

    transports       text[]  NOT NULL DEFAULT '{}',
    backup_eligible  boolean NOT NULL DEFAULT false,
    backup_state     boolean NOT NULL DEFAULT false,

    -- Human label so a user can tell which key to revoke ("MacBook", "YubiKey").
    name             text NOT NULL DEFAULT '',

    created_at       timestamptz NOT NULL DEFAULT now(),
    last_used_at     timestamptz
);

-- Credential IDs are globally unique by construction; enforcing it prevents
-- one authenticator being bound to two accounts.
CREATE UNIQUE INDEX webauthn_credentials_credential_id_key ON webauthn_credentials (credential_id);
CREATE INDEX webauthn_credentials_user_id_idx ON webauthn_credentials (user_id);

-- ---------------------------------------------------------------------------
-- recovery_codes
-- ---------------------------------------------------------------------------
-- The answer to "I lost my only passkey". Stored as argon2id hashes: a leaked
-- database must not yield working credentials.
CREATE TABLE recovery_codes (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  text NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX recovery_codes_user_id_idx ON recovery_codes (user_id);

-- ---------------------------------------------------------------------------
-- sessions
-- ---------------------------------------------------------------------------
-- Server-side sessions, not stateless JWTs. Revocation must be immediate: a
-- JWT stays valid until it expires no matter how urgently you want it dead,
-- which is the wrong trade for "revoke my stolen laptop right now".
CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Only the SHA-256 of the token is stored. Database read access must not
    -- hand over live sessions.
    token_hash   bytea NOT NULL,

    -- web       — browser cookie session
    -- device    — long-lived token for WebDAV/CLI clients (slice 6)
    -- recovery  — created by redeeming a recovery code; deliberately
    --             short-lived, because its only job is to let you enrol a new
    --             passkey.
    kind         text NOT NULL DEFAULT 'web',

    user_agent   text NOT NULL DEFAULT '',
    ip           inet,

    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz
);

CREATE UNIQUE INDEX sessions_token_hash_key ON sessions (token_hash);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
-- Supports the periodic sweep of expired rows.
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- ---------------------------------------------------------------------------
-- webauthn_ceremonies — in-flight registration/login challenges
-- ---------------------------------------------------------------------------
-- A WebAuthn ceremony spans two requests, and the challenge issued by the
-- first must be verified by the second. Keeping it server-side (rather than in
-- a cookie the client controls) means the client cannot choose its own
-- challenge. Rows are short-lived and swept.
CREATE TABLE webauthn_ceremonies (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- NULL during bootstrap registration, when the user does not exist yet.
    user_id      uuid REFERENCES users (id) ON DELETE CASCADE,

    kind         text  NOT NULL,   -- 'registration' | 'login'
    session_data jsonb NOT NULL,

    -- Carried through bootstrap registration, where there is no user row yet
    -- to read the username from.
    pending_username text,

    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);

CREATE INDEX webauthn_ceremonies_expires_at_idx ON webauthn_ceremonies (expires_at);

-- +goose Down
DROP TABLE IF EXISTS webauthn_ceremonies;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS recovery_codes;
DROP TABLE IF EXISTS webauthn_credentials;
DROP TABLE IF EXISTS users;
