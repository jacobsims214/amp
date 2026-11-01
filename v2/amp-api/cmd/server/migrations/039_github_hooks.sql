-- GitHub merge webhook (AMP #2479, KB decisions/human-work-ux.md §4).
--
-- A project can receive GitHub's pull_request events at
-- POST /api/hooks/github/{project_id}. Each request is authenticated only by
-- X-Hub-Signature-256, an HMAC over the body under the project's own secret.
-- The secret is stored sealed by secretbox (purpose
-- project_github_hooks.secret), as webhook signing secrets are. Without an
-- encryption key it is stored in plaintext and sealed at the next startup
-- that has one.
--
-- github_hook_deliveries makes deliveries idempotent by X-GitHub-Delivery
-- and is the small log the settings page shows (the last 50 per project).
-- Only deliveries with a valid signature are logged. Rejected ones only
-- count up rejected_count/last_rejected_at, so an unauthenticated caller
-- can't push real deliveries out of the log.
--
-- Additive only. Idempotent.
CREATE TABLE IF NOT EXISTS project_github_hooks (
    project_id       INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    secret           TEXT NOT NULL,
    created_by       INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rotated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rejected_count   BIGINT NOT NULL DEFAULT 0,
    last_rejected_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS github_hook_deliveries (
    id          BIGSERIAL PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    delivery_id TEXT NOT NULL,
    event       TEXT NOT NULL,
    action      TEXT NOT NULL DEFAULT '',
    -- 0 while it is being handled; a delivery that ended in a 5xx may be
    -- redelivered and handled again.
    status      INTEGER NOT NULL DEFAULT 0,
    result      TEXT NOT NULL DEFAULT '',
    task_ids    INTEGER[] NOT NULL DEFAULT '{}',
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, delivery_id)
);

CREATE INDEX IF NOT EXISTS idx_github_hook_deliveries_recent
    ON github_hook_deliveries(project_id, received_at DESC, id DESC);
