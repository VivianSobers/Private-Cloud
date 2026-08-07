-- +goose Up
-- Phase 4, slice 4: OIDC identities.
--
-- Single sign-on is an ADDITIONAL door, never a replacement: passkeys stay the
-- primary, phishing-resistant credential, and the admin is still bootstrapped by
-- a passkey. This table maps an external identity — a provider's issuer + the
-- stable subject it assigns — to a local user, so a returning SSO user lands on
-- the same account each time.
--
-- Keyed by (issuer, subject), the only identifier a provider promises is stable.
-- Email is stored for display and domain policy but is NOT the key: emails change
-- and get reassigned, and keying on one would let a reassigned address inherit
-- someone else's files. OIDC provisions its OWN users rather than auto-linking to
-- a passkey account by email, which removes email-based account takeover as a
-- risk entirely.

CREATE TABLE oidc_identities (
    issuer  text NOT NULL,
    subject text NOT NULL,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    email   text NOT NULL DEFAULT '',

    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,

    PRIMARY KEY (issuer, subject)
);

CREATE INDEX oidc_identities_user ON oidc_identities (user_id);

-- +goose Down
DROP TABLE IF EXISTS oidc_identities;
