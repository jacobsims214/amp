-- Agent accounts, deactivation, and API tokens.
--
-- An agent account is a users row of kind 'agent', managed by the person
-- who created it. It never signs in through Dex; it authenticates with API
-- tokens. Any account can be deactivated (disabled_at), which refuses its
-- requests without touching its work or history.
ALTER TABLE users ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'human';
ALTER TABLE users ADD COLUMN IF NOT EXISTS managed_by INT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE users ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS api_tokens (
    id           SERIAL PRIMARY KEY,
    user_id      INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE, -- sha256 of the token; the token itself is never stored
    prefix       TEXT NOT NULL,
    created_by   INT REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens(user_id);
