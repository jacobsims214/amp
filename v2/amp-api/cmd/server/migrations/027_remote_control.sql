-- Remote control of a harness session from the web app (epic 165, AMP #2371).
-- Spec: v2/docs/remote-control/protocol.md; decisions/harness-remote-control.md.
--
-- Additive and idempotent: three new org columns with defaults (remote
-- control is OFF for every org until an org admin turns it on), and two new
-- tables. Nothing existing changes, and running it again changes nothing.
--
-- No message content is ever stored here: the relay keeps chat payloads in
-- memory only, and the audit log holds metadata (D3).

-- The org setting (protocol 8.3 "Org policy"): off by default.
ALTER TABLE orgs ADD COLUMN IF NOT EXISTS remote_control_enabled BOOLEAN NOT NULL DEFAULT FALSE;
-- Minutes without remote activity before a session ends (5..240).
ALTER TABLE orgs ADD COLUMN IF NOT EXISTS remote_idle_timeout_min INT NOT NULL DEFAULT 30;
-- How long a paired device stays trusted, in days (0 = re-pair every session, max 30).
ALTER TABLE orgs ADD COLUMN IF NOT EXISTS remote_device_pin_days INT NOT NULL DEFAULT 30;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'orgs_remote_idle_timeout_min_range') THEN
        ALTER TABLE orgs ADD CONSTRAINT orgs_remote_idle_timeout_min_range
            CHECK (remote_idle_timeout_min BETWEEN 5 AND 240);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'orgs_remote_device_pin_days_range') THEN
        ALTER TABLE orgs ADD CONSTRAINT orgs_remote_device_pin_days_range
            CHECK (remote_device_pin_days BETWEEN 0 AND 30);
    END IF;
END $$;

-- The session registry: one row per `/remote` on a laptop. An ended session
-- never comes back (a new `/remote` registers a new one).
CREATE TABLE IF NOT EXISTS remote_sessions (
    id                         TEXT PRIMARY KEY,                     -- "rs_" + 22 b64u chars
    owner_user_id              INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id                 INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    host                       TEXT NOT NULL DEFAULT '',
    os                         TEXT NOT NULL DEFAULT '',
    harness_version            TEXT NOT NULL DEFAULT '',
    harness_id                 TEXT NOT NULL,                        -- fingerprint of harness_pub
    harness_pub                TEXT NOT NULL,                        -- b64u raw P-256 point (public)
    protocol_version           INT NOT NULL DEFAULT 1,
    -- The amp-api replica (api_nodes.id) holding the harness's command stream.
    holder_node                TEXT,
    state                      TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'ended')),
    end_reason                 TEXT,
    -- SHA-256 (hex) of the current connect token; the token itself is never stored.
    connect_token_hash         TEXT,
    connect_token_expires_at   TIMESTAMPTZ,
    started_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    heartbeat_at               TIMESTAMPTZ,
    last_activity_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at                 TIMESTAMPTZ NOT NULL,                 -- started_at + 12 h
    ended_at                   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS remote_sessions_owner_active ON remote_sessions (owner_user_id) WHERE state = 'active';
CREATE INDEX IF NOT EXISTS remote_sessions_project_active ON remote_sessions (project_id) WHERE state = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS remote_sessions_connect_token ON remote_sessions (connect_token_hash)
    WHERE connect_token_hash IS NOT NULL;

-- Devices (browsers) paired with a harness install, as the relay learned them
-- from signed pair_result events. Used for early checks only; the harness's
-- own pins decide trust (protocol 7.6).
CREATE TABLE IF NOT EXISTS remote_devices (
    id            BIGSERIAL PRIMARY KEY,
    owner_user_id INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    harness_id    TEXT NOT NULL,
    device_id     TEXT NOT NULL,                                     -- fingerprint of device_pub
    device_pub    TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    paired_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '30 days',
    revoked_at    TIMESTAMPTZ,
    UNIQUE (owner_user_id, harness_id, device_id)
);
CREATE INDEX IF NOT EXISTS remote_devices_owner ON remote_devices (owner_user_id) WHERE revoked_at IS NULL;
