-- AMP v2 schema — users & roles
-- Added when moving auth to Dex (OIDC). Identity/credentials live in Dex;
-- this table is for JIT-provisioned identity + role assignment inside amp-api.
-- Safe to re-run (all CREATE IF NOT EXISTS).

CREATE TABLE IF NOT EXISTS users (
    id           SERIAL PRIMARY KEY,
    subject      TEXT NOT NULL UNIQUE,  -- OIDC "sub" claim from Dex — stable identity key
    email        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_roles (
    user_id  INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role     TEXT NOT NULL, -- 'admin' | 'member'
    PRIMARY KEY (user_id, role)
);

CREATE INDEX IF NOT EXISTS idx_user_roles_user_id ON user_roles(user_id);
