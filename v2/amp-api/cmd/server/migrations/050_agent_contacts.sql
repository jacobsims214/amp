-- First-run checklist (#128): "Connect an agent" is checked once an agent
-- has reached AMP's MCP endpoint as you, and stays checked.
--
-- One row per caller, keyed 'user:<id>' for an AMP user (a person or an
-- agent account) and 'local-dev' when sign-in is off and there's no user.
-- managed_by is the agent account's person, so a person's checklist also
-- counts the agent accounts they manage. first_at and client never change; last_at is
-- refreshed at most once a minute.
--
-- API tokens already keep last_used_at, which the checklist reads too; this
-- table covers browser-login harnesses and local dev, which use no token.
--
-- Additive only; idempotent.

CREATE TABLE IF NOT EXISTS agent_contacts (
    principal   TEXT PRIMARY KEY,
    user_id     INT REFERENCES users(id) ON DELETE CASCADE,
    managed_by  INT REFERENCES users(id) ON DELETE SET NULL,
    client      TEXT NOT NULL DEFAULT '',
    first_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_contacts_managed_by ON agent_contacts(managed_by) WHERE managed_by IS NOT NULL;
