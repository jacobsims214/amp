-- Webhooks: AMP POSTs events (ticket changes, comments, and optionally
-- audit entries) to a URL, for a project or for every project in an org.
-- Deliveries are an outbox: written when the event happens, sent and
-- retried by a background dispatcher, kept for a while for inspection.
CREATE TABLE IF NOT EXISTS webhooks (
    id                   SERIAL PRIMARY KEY,
    org_id               INT REFERENCES orgs(id) ON DELETE CASCADE,
    project_id           INT REFERENCES projects(id) ON DELETE CASCADE,
    name                 TEXT NOT NULL DEFAULT '',
    url                  TEXT NOT NULL,
    format               TEXT NOT NULL DEFAULT 'json', -- 'json' | 'slack'
    secret               TEXT NOT NULL,                -- HMAC key for X-AMP-Signature
    events               TEXT[] NOT NULL DEFAULT '{}', -- patterns; empty = every project event
    active               BOOLEAN NOT NULL DEFAULT TRUE,
    created_by           INT REFERENCES users(id) ON DELETE SET NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_delivery_at     TIMESTAMPTZ,
    last_status          INT,
    last_error           TEXT NOT NULL DEFAULT '',
    consecutive_failures INT NOT NULL DEFAULT 0,
    CHECK ((org_id IS NULL) <> (project_id IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_webhooks_project ON webhooks(project_id) WHERE active;
CREATE INDEX IF NOT EXISTS idx_webhooks_org ON webhooks(org_id) WHERE active;

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id              BIGSERIAL PRIMARY KEY,
    webhook_id      INT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL,
    body            TEXT NOT NULL,                     -- exactly what is POSTed (and signed)
    status          TEXT NOT NULL DEFAULT 'pending',   -- 'pending' | 'delivered' | 'failed'
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_status     INT,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_due ON webhook_deliveries(next_attempt_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_hook ON webhook_deliveries(webhook_id, id DESC);
