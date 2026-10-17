-- Running more than one amp-api replica. Each project's actors run on
-- exactly one replica (its owner); the others pass that project's requests
-- to the owner. See internal/cluster.

-- Every running replica, and when it last proved it is alive.
CREATE TABLE IF NOT EXISTS api_nodes (
    id           TEXT PRIMARY KEY,
    addr         TEXT NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Which replica runs each project's actors. A row whose node has stopped
-- heartbeating can be taken over; epoch counts the takeovers.
CREATE TABLE IF NOT EXISTS project_owners (
    project_id  INT PRIMARY KEY,
    node_id     TEXT NOT NULL,
    epoch       BIGINT NOT NULL DEFAULT 1,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_project_owners_node ON project_owners(node_id);

-- Values every replica shares, e.g. the token replicas use with each other.
CREATE TABLE IF NOT EXISTS cluster_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Live events too big for a NOTIFY payload; replicas read them by id.
CREATE TABLE IF NOT EXISTS live_events (
    id   BIGSERIAL PRIMARY KEY,
    body TEXT NOT NULL,
    at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
