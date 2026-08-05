-- +goose Up
-- Phase 2, slice 4: public share links.
--
-- The first thing in this system reachable WITHOUT an account, so it gets its
-- own table, its own surface, and its own rules. A share is a row — never a
-- signed token — because revocation has to be immediate: deleting the row kills
-- the link on the very next request, with no window where a still-valid
-- signature outlives the owner's decision to revoke.

CREATE TABLE shares (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- What is shared, and by whom. Both CASCADE: purging the file or deleting
    -- the account takes its links with it. Trashing the file does NOT delete the
    -- row, so serving must re-check the node is live — a share must never resurrect
    -- a trashed file into public view.
    node_id  uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    owner_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- SHA-256 of the URL token, never the token itself. The token is 256 bits of
    -- CSPRNG output, so there is nothing to brute-force and no argon2 is needed —
    -- but hashing at rest means a database leak does not hand over working links.
    token_hash bytea NOT NULL CHECK (octet_length(token_hash) = 32),

    -- A per-share secret, generated at creation and NEVER sent to any client.
    -- After a password is verified the server issues an HMAC over this key; the
    -- content request carries that HMAC back, proving the password was checked
    -- without the server keeping per-visitor session state.
    unlock_key bytea NOT NULL CHECK (octet_length(unlock_key) = 32),

    -- Optional password, argon2id (PHC string), same as every other human secret
    -- in this system. NULL means the link alone is sufficient.
    password_hash text,

    -- Optional expiry and optional download cap. NULL means no limit.
    expires_at    timestamptz,
    max_downloads bigint CHECK (max_downloads IS NULL OR max_downloads > 0),
    download_count bigint NOT NULL DEFAULT 0 CHECK (download_count >= 0),

    created_at timestamptz NOT NULL DEFAULT now(),

    -- Revocation is a timestamp, not a delete, so a revoked link reports "gone"
    -- distinctly from one that never existed, and so the owner's share list can
    -- still show what they revoked and when. GC removes the rows later.
    revoked_at timestamptz
);

-- The lookup every public request makes: token -> share. Unique so two shares
-- cannot collide on a token (astronomically unlikely, but the constraint makes
-- it impossible rather than merely improbable).
CREATE UNIQUE INDEX shares_token_hash_idx ON shares (token_hash);

-- The owner's "my links" listing.
CREATE INDEX shares_owner_idx ON shares (owner_id, created_at DESC);
-- "What links point at this node", for showing share state on a file.
CREATE INDEX shares_node_idx ON shares (node_id);
-- GC of shares past their expiry.
CREATE INDEX shares_expiry_idx ON shares (expires_at) WHERE expires_at IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS shares;
