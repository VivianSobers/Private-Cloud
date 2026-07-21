-- +goose Up
-- Phase 1, slice 6: credentials for clients that cannot do WebAuthn.
--
-- WebDAV is the reason this table exists. A Finder, Explorer or rclone mount
-- speaks HTTP Basic and nothing else — there is no way to run a passkey
-- ceremony from a filesystem driver. So those clients get a per-client secret
-- instead.
--
-- This is a real weakening of the auth model and is treated as such:
--
--   * One password per client, named, individually revocable. Losing a laptop
--     revokes one credential rather than forcing a global reset.
--   * Stored as argon2id, never recoverable — shown exactly once at creation,
--     like recovery codes.
--   * Scoped to /dav only. An app password cannot call the JSON API, so a
--     leaked one cannot enrol a passkey, read recovery codes, or change auth
--     settings. It can reach files, which is bad enough; it cannot escalate to
--     owning the account.
--   * Optional expiry, because a credential handed to a device you no longer
--     control should be able to die on its own.

CREATE TABLE app_passwords (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Shown in the UI so a user can tell which client to revoke.
    name    text NOT NULL CHECK (name <> ''),

    -- PHC-format argon2id, same parameters as recovery codes.
    secret_hash text NOT NULL,

    -- A short, non-secret prefix of the plaintext. Lets verification look up
    -- ONE candidate row instead of argon2-hashing every password the user owns
    -- on every single WebDAV request — and a WebDAV client makes a great many
    -- requests. Not secret: it is only an index, and the argon2 verification
    -- behind it is what actually authenticates.
    lookup_id text NOT NULL,

    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    expires_at   timestamptz,
    revoked_at   timestamptz
);

CREATE UNIQUE INDEX app_passwords_lookup_key ON app_passwords (lookup_id);
CREATE INDEX app_passwords_user_idx ON app_passwords (user_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS app_passwords;
