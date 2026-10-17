-- Audit log: who changed access, settings, or deleted things, and when.
-- Append-only: nothing in AMP updates or deletes these rows.
CREATE TABLE IF NOT EXISTS audit_log (
    id            BIGSERIAL PRIMARY KEY,
    at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    actor_user_id INT REFERENCES users(id) ON DELETE SET NULL,
    actor_email   TEXT NOT NULL DEFAULT '', -- kept even if the user is deleted
    via_token     BOOLEAN NOT NULL DEFAULT FALSE,
    org_id        INT,  -- no foreign keys: entries outlive what they describe
    project_id    INT,
    action        TEXT NOT NULL,
    target_type   TEXT NOT NULL DEFAULT '',
    target_id     TEXT NOT NULL DEFAULT '',
    summary       TEXT NOT NULL DEFAULT '',
    details       JSONB NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_audit_org ON audit_log(org_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_project ON audit_log(project_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log(actor_user_id, id DESC);
